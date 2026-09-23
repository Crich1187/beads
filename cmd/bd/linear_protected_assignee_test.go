package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/beads/internal/tracker"
)

type protectedAssigneeConfigReader struct {
	values map[string]string
	err    error
}

func (r protectedAssigneeConfigReader) GetConfig(_ context.Context, key string) (string, error) {
	return r.values[key], r.err
}

func TestApplyLinearProtectedAssigneeConfig(t *testing.T) {
	const key = "linear.protected_assignee_patterns"
	for _, tc := range []struct {
		name    string
		reader  protectedAssigneeConfigReader
		want    []string
		wantErr bool
	}{
		{name: "unset", reader: protectedAssigneeConfigReader{}, want: nil},
		{name: "blank", reader: protectedAssigneeConfigReader{values: map[string]string{key: "  "}}, want: nil},
		{name: "empty array", reader: protectedAssigneeConfigReader{values: map[string]string{key: "[]"}}, want: []string{}},
		{
			name:   "json array keeps commas inside patterns",
			reader: protectedAssigneeConfigReader{values: map[string]string{key: `["athena(?:\\s*\\(.*\\))?|hermes", "swarm-qa-[0-9]{1,3}"]`}},
			want:   []string{`athena(?:\s*\(.*\))?|hermes`, `swarm-qa-[0-9]{1,3}`},
		},
		{name: "comma list is rejected", reader: protectedAssigneeConfigReader{values: map[string]string{key: "athena,pepper"}}, wantErr: true},
		{name: "read error fails closed", reader: protectedAssigneeConfigReader{err: errors.New("db down")}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opts tracker.SyncOptions
			err := applyLinearProtectedAssigneeConfig(context.Background(), tc.reader, &opts)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && !reflect.DeepEqual(opts.ProtectedAssigneePatterns, tc.want) {
				t.Fatalf("patterns = %#v, want %#v", opts.ProtectedAssigneePatterns, tc.want)
			}
		})
	}
}
