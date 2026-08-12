package biz

import (
	"context"
	"fmt"
	"strings"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/transformer"
)

type VideoService struct {
	ChannelService *ChannelService
	RequestService *RequestService
}

func NewVideoService(channelService *ChannelService, requestService *RequestService) *VideoService {
	return &VideoService{
		ChannelService: channelService,
		RequestService: requestService,
	}
}

func (s *VideoService) GetTask(ctx context.Context, requestID int) (*llm.Response, error) {
	task, ch, outbound, err := s.loadTask(ctx, requestID)
	if err != nil {
		return nil, err
	}

	httpReq, err := outbound.BuildGetVideoTaskRequest(ctx, task.ExternalID)
	if err != nil {
		return nil, err
	}

	httpResp, err := ch.HTTPClient.Do(ctx, httpReq)
	if err != nil {
		return nil, err
	}

	video, err := outbound.ParseGetVideoTaskResponse(ctx, httpResp)
	if err != nil {
		return nil, err
	}

	// Persist snapshot to request table for task tracking.
	status := mapVideoStatusToRequestStatus(video.Video.Status)

	// Always store the latest response snapshot.
	if err := s.RequestService.UpdateRequestStatusExternalIDAndResponseBody(ctx, requestID, status, task.ExternalID, video.Video, nil); err != nil {
		// non-fatal: return data anyway
	}

	return video, nil
}

// scopeTaskQueryToProject restricts a video task lookup to the caller's
// project.
//
// The Request privacy policy grants API-key principals with write_requests
// scope table-wide access, so project ownership must be enforced here: these
// lookups back the API-key-authenticated GET/DELETE /v1/videos/:id and doubao
// task endpoints, and without the project filter any API key could read or
// cancel another project's tasks by guessing provider task IDs.
//
// Callers without a project are rejected rather than given the unfiltered
// query. API-key auth always installs the key's project, and background flows
// such as the video storage worker run under a bypass, so a context with
// neither is a wiring mistake — for example a future admin or JWT route for
// videos — and must not silently widen the lookup to every project.
func scopeTaskQueryToProject(ctx context.Context, query *ent.RequestQuery) (*ent.RequestQuery, error) {
	if projectID, ok := contexts.GetProjectID(ctx); ok {
		return query.Where(request.ProjectID(projectID)), nil
	}

	if authz.IsBypassActive(ctx) {
		return query, nil
	}

	return nil, fmt.Errorf("%w: video task lookup requires a project in context", ErrInternal)
}

// findTaskByExternalID resolves a video task by the provider's task ID
// (external_id), scoped to the caller's project.
func (s *VideoService) findTaskByExternalID(ctx context.Context, externalID string) (*ent.Request, error) {
	client := ent.FromContext(ctx)
	if client == nil {
		return nil, fmt.Errorf("%w: ent client not found in context", ErrInternal)
	}

	// Cross-project lookups must behave exactly like a missing task.
	query, err := scopeTaskQueryToProject(ctx, client.Request.Query().Where(request.ExternalID(externalID)))
	if err != nil {
		return nil, err
	}

	return query.Only(ctx)
}

// GetTaskByExternalID looks up a video task by the provider's task ID (external_id).
// NOTE: assumes provider task IDs are globally unique across channels.
func (s *VideoService) GetTaskByExternalID(ctx context.Context, externalID string) (*llm.Response, error) {
	task, err := s.findTaskByExternalID(ctx, externalID)
	if err != nil {
		return nil, err
	}

	return s.GetTask(ctx, task.ID)
}

// DeleteTaskByExternalID deletes a video task by the provider's task ID (external_id).
// NOTE: assumes provider task IDs are globally unique across channels.
func (s *VideoService) DeleteTaskByExternalID(ctx context.Context, externalID string) error {
	task, err := s.findTaskByExternalID(ctx, externalID)
	if err != nil {
		return err
	}

	return s.DeleteTask(ctx, task.ID)
}

func (s *VideoService) DeleteTask(ctx context.Context, requestID int) error {
	task, ch, outbound, err := s.loadTask(ctx, requestID)
	if err != nil {
		return err
	}

	httpReq, err := outbound.BuildDeleteVideoTaskRequest(ctx, task.ExternalID)
	if err != nil {
		return err
	}

	_, err = ch.HTTPClient.Do(ctx, httpReq)
	if err != nil {
		return err
	}

	// Best effort: mark canceled locally.
	_ = s.RequestService.UpdateRequestStatus(ctx, requestID, request.StatusCanceled)

	return nil
}

func (s *VideoService) loadTask(ctx context.Context, requestID int) (*ent.Request, *Channel, transformer.VideoTaskOutbound, error) {
	client := ent.FromContext(ctx)
	if client == nil {
		return nil, nil, nil, fmt.Errorf("%w: ent client not found in context", ErrInternal)
	}

	query, err := scopeTaskQueryToProject(ctx, client.Request.Query().Where(request.ID(requestID)))
	if err != nil {
		return nil, nil, nil, err
	}

	task, err := query.Only(ctx)
	if err != nil {
		return nil, nil, nil, err
	}

	if strings.TrimSpace(task.ExternalID) == "" {
		return nil, nil, nil, fmt.Errorf("%w: missing external_id for task", ErrInternal)
	}

	if task.ChannelID == 0 {
		return nil, nil, nil, fmt.Errorf("%w: missing channel_id for task", ErrInternal)
	}

	ch, err := s.ChannelService.GetChannel(ctx, task.ChannelID)
	if err != nil {
		return nil, nil, nil, err
	}

	var outbound transformer.VideoTaskOutbound

	var videoKey string

	switch ch.Type {
	case channel.TypeDoubao:
		videoKey = llm.APIFormatSeedanceVideo.String()
	default:
		videoKey = llm.APIFormatOpenAIVideo.String()
	}

	if ch.Outbounds != nil {
		if out, ok := ch.Outbounds[videoKey]; ok {
			outbound, _ = out.(transformer.VideoTaskOutbound)
		}
	}

	if outbound == nil {
		return nil, nil, nil, fmt.Errorf("%w: channel does not support video task operations", ErrInternal)
	}

	return task, ch, outbound, nil
}

func mapVideoStatusToRequestStatus(status string) request.Status {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "succeeded":
		return request.StatusCompleted
	case "failed":
		return request.StatusFailed
	case "queued", "running":
		return request.StatusProcessing
	default:
		return request.StatusProcessing
	}
}
