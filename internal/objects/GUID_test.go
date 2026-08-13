package objects

import (
	"bytes"
	"testing"
)

func TestGUID_MarshalGQL(t *testing.T) {
	type fields struct {
		Type string
		UUID int
	}

	tests := []struct {
		name   string
		fields fields
		wantW  string
	}{
		{
			name: "gid",
			fields: fields{
				Type: "type",
				UUID: 1,
			},
			wantW: `"gid://axonhub/type/1"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			guid := GUID{
				Type: tt.fields.Type,
				ID:   tt.fields.UUID,
			}
			w := &bytes.Buffer{}
			guid.MarshalGQL(w)

			if gotW := w.String(); gotW != tt.wantW {
				t.Errorf("GUID.MarshalGQL() = %v, want %v", gotW, tt.wantW)
			}
		})
	}
}

func TestGUID_UnmarshalGQL(t *testing.T) {
	type fields struct {
		Type string
		ID   int
	}

	type args struct {
		v any
	}

	tests := []struct {
		name    string
		fields  fields
		args    args
		want    GUID
		wantErr bool
	}{
		{
			name: "gid",
			fields: fields{
				Type: "type",
				ID:   1,
			},
			args: args{
				v: "gid://axonhub/type/1",
			},
			want: GUID{Type: "type", ID: 1},
		},
		{
			name: "empty",
			fields: fields{
				Type: "",
				ID:   0,
			},
			args: args{
				v: "",
			},
			wantErr: true,
		},
		{
			name: "invalid",
			fields: fields{
				Type: "type",
				ID:   0,
			},
			args: args{
				v: "gid://axonhub/type/invalid",
			},
			wantErr: true,
		},
		{
			name: "invalid prefix",
			fields: fields{
				Type: "type",
				ID:   0,
			},
			args: args{
				v: "guid://invalid/1",
			},
			wantErr: true,
		},
		{
			name: "old format should fail",
			fields: fields{
				Type: "type",
				ID:   0,
			},
			args: args{
				v: "gid://type/1",
			},
			wantErr: true,
		},
		{
			name: "missing axonhub namespace",
			fields: fields{
				Type: "type",
				ID:   0,
			},
			args: args{
				v: "gid://other/type/1",
			},
			wantErr: true,
		},
		{
			name: "empty type",
			fields: fields{
				Type: "",
				ID:   0,
			},
			args: args{
				v: "gid://axonhub//1",
			},
			wantErr: true,
		},
		{
			name: "zero id is accepted",
			fields: fields{
				Type: "other",
				ID:   7,
			},
			args: args{
				v: "gid://axonhub/type/0",
			},
			want: GUID{Type: "type", ID: 0},
		},
		{
			name: "negative id",
			fields: fields{
				Type: "type",
				ID:   0,
			},
			args: args{
				v: "gid://axonhub/type/-1",
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			guid := &GUID{
				Type: tt.fields.Type,
				ID:   tt.fields.ID,
			}

			err := guid.UnmarshalGQL(tt.args.v)
			if (err != nil) != tt.wantErr {
				t.Errorf("GUID.UnmarshalGQL() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.wantErr {
				return
			}

			if *guid != tt.want {
				t.Errorf("GUID.UnmarshalGQL() = %+v, want %+v", *guid, tt.want)
			}
		})
	}
}

// TestGUID_UnmarshalGQL_ZeroIDSystemSentinel pins the zero id down as valid.
// System-level roles are stored with project_id = 0 (see biz/role.go, which
// queries them with role.ProjectIDEQ(0)), and the dashboard roles list filters
// on that sentinel by sending projectID: "gid://axonhub/Project/0". Rejecting
// zero here breaks the roles page with a GraphQL input coercion error.
func TestGUID_UnmarshalGQL_ZeroIDSystemSentinel(t *testing.T) {
	guid, err := ParseGUID("gid://axonhub/Project/0")
	if err != nil {
		t.Fatalf("ParseGUID() error = %v, want nil", err)
	}

	if guid.Type != "Project" || guid.ID != 0 {
		t.Fatalf("ParseGUID() = %+v, want {Type:Project ID:0}", guid)
	}

	var w bytes.Buffer

	guid.MarshalGQL(&w)

	if got := w.String(); got != `"gid://axonhub/Project/0"` {
		t.Errorf("GUID.MarshalGQL() = %s, want %q", got, "gid://axonhub/Project/0")
	}
}
