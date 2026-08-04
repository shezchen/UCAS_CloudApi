package llm

import "encoding/json"

// ProviderExtensions carries provider/API-format private data that should not
// be serialized through the common llm request/response JSON model.
type ProviderExtensions struct {
	OpenAIResponses *OpenAIResponsesProviderExtensions `json:"-"`
}

type OpenAIResponsesProviderExtensions struct {
	Request  *OpenAIResponsesRequestExtensions  `json:"-"`
	Response *OpenAIResponsesResponseExtensions `json:"-"`
}

// OpenAIResponsesResponseExtensions carries the original ordered Responses
// output items across the unified response model. RawOutputItems is deliberately
// non-serializable through llm.Response so response content is not duplicated in
// persisted transformer metadata.
type OpenAIResponsesResponseExtensions struct {
	RawOutputItems []json.RawMessage `json:"-"`
}

type OpenAIResponsesRequestExtensions struct {
	RawTools       []OpenAIResponsesRawFragment `json:"-"`
	ToolSignatures []string                     `json:"-"`
	RawToolChoice  json.RawMessage              `json:"-"`
	RawInputItems  []OpenAIResponsesRawFragment `json:"-"`
}

type OpenAIResponsesRawFragment struct {
	Type          string          `json:"-"`
	Name          string          `json:"-"`
	CallID        string          `json:"-"`
	OriginalIndex int             `json:"-"`
	Raw           json.RawMessage `json:"-"`
}

func EnsureOpenAIResponsesProviderExtensions(req *Request) *OpenAIResponsesProviderExtensions {
	if req == nil {
		return nil
	}

	if req.ProviderExtensions == nil {
		req.ProviderExtensions = &ProviderExtensions{}
	}

	if req.ProviderExtensions.OpenAIResponses == nil {
		req.ProviderExtensions.OpenAIResponses = &OpenAIResponsesProviderExtensions{}
	}

	return req.ProviderExtensions.OpenAIResponses
}

func CloneProviderExtensions(src *ProviderExtensions) *ProviderExtensions {
	if src == nil {
		return nil
	}

	cloned := &ProviderExtensions{}
	if src.OpenAIResponses != nil {
		cloned.OpenAIResponses = &OpenAIResponsesProviderExtensions{}
		if src.OpenAIResponses.Request != nil {
			cloned.OpenAIResponses.Request = &OpenAIResponsesRequestExtensions{
				RawTools:       cloneOpenAIResponsesRawFragments(src.OpenAIResponses.Request.RawTools),
				ToolSignatures: append([]string(nil), src.OpenAIResponses.Request.ToolSignatures...),
				RawToolChoice:  cloneRawMessage(src.OpenAIResponses.Request.RawToolChoice),
				RawInputItems:  cloneOpenAIResponsesRawFragments(src.OpenAIResponses.Request.RawInputItems),
			}
		}
		if src.OpenAIResponses.Response != nil {
			cloned.OpenAIResponses.Response = &OpenAIResponsesResponseExtensions{
				RawOutputItems: cloneRawMessages(src.OpenAIResponses.Response.RawOutputItems),
			}
		}
	}

	return cloned
}

func cloneRawMessages(src []json.RawMessage) []json.RawMessage {
	if len(src) == 0 {
		return nil
	}

	out := make([]json.RawMessage, len(src))
	for i := range src {
		out[i] = cloneRawMessage(src[i])
	}

	return out
}

func cloneOpenAIResponsesRawFragments(src []OpenAIResponsesRawFragment) []OpenAIResponsesRawFragment {
	if len(src) == 0 {
		return nil
	}

	out := make([]OpenAIResponsesRawFragment, len(src))
	for i := range src {
		out[i] = src[i]
		out[i].Raw = cloneRawMessage(src[i].Raw)
	}

	return out
}

func cloneRawMessage(src json.RawMessage) json.RawMessage {
	if len(src) == 0 {
		return nil
	}

	return append(json.RawMessage(nil), src...)
}
