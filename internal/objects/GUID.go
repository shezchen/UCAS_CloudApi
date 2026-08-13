package objects

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/samber/lo"
)

type GUID struct {
	Type string `json:"type"`
	ID   int    `json:"id"`
}

func (guid GUID) MarshalGQL(w io.Writer) {
	_, _ = io.WriteString(w, strconv.Quote(fmt.Sprintf("gid://axonhub/%s/%d", guid.Type, guid.ID)))
}

func (guid *GUID) UnmarshalGQL(v any) error {
	str, ok := v.(string)
	if !ok {
		return fmt.Errorf("enum %T must be a string", v)
	}

	if str == "" {
		return errors.New("guid is empty")
	}

	if !strings.HasPrefix(str, "gid://axonhub/") {
		return errors.New("guid must start with gid://axonhub/")
	}

	str = str[14:] // Remove "gid://axonhub/" prefix

	before, after, ok0 := strings.Cut(str, "/")
	if !ok0 {
		return errors.New("guid must contain type and id")
	}

	typ := before
	if typ == "" {
		return errors.New("guid type must not be empty")
	}

	id, err := strconv.Atoi(after)
	if err != nil {
		return err
	}

	// Zero is a legitimate id: system-level rows use it as a sentinel for
	// "no project" (see the project_id = 0 system roles in biz/role.go), and
	// the dashboard filters on gid://axonhub/Project/0.
	if id < 0 {
		return errors.New("guid id must not be negative")
	}

	guid.Type = typ
	guid.ID = id

	return nil
}

func ParseGUID(str string) (GUID, error) {
	var guid GUID

	err := guid.UnmarshalGQL(str)
	if err != nil {
		return GUID{}, err
	}

	return guid, nil
}

// ConvertGUIDToInt converts a GUID to an int id.
//
// NOTE: The ConvertGUID* converters are invoked from gqlgen-generated code
// (see the converters section in internal/server/gql/gqlgen.yml), which does
// not carry the expected entity type, so the type segment cannot be validated
// here. Resolvers that accept a GUID for a specific entity must check
// guid.Type against the expected ent type themselves before using the id
// (see e.g. the APIKey validation in dashboard.resolvers.go).
func ConvertGUIDToInt(guid GUID) (int, error) {
	return guid.ID, nil
}

func ConvertGUIDPtrToInt(guid *GUID) (int, error) {
	if guid == nil {
		return 0, errors.New("guid is nil")
	}

	return guid.ID, nil
}

func ConvertGUIDToIntPtr(guid GUID) (*int, error) {
	return lo.ToPtr(guid.ID), nil
}

func ConvertGUIDPtrToIntPtr(guid *GUID) (*int, error) {
	if guid == nil {
		return nil, errors.New("guid is nil")
	}

	return lo.ToPtr(guid.ID), nil
}

func ConvertGUIDPtrsToIntPtrs(guid []*GUID) ([]*int, error) {
	return lo.Map(guid, func(item *GUID, index int) *int {
		return lo.ToPtr(item.ID)
	}), nil
}

func ConvertGUIDPtrsToInts(guid []*GUID) ([]int, error) {
	return lo.Map(guid, func(item *GUID, index int) int {
		return item.ID
	}), nil
}

func IntGuids(guids []*GUID) []int {
	return lo.Map(guids, func(item *GUID, index int) int { return item.ID })
}
