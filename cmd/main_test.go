package main

import (
	"strings"
	"testing"
)

func TestConfigPathFromArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{
			name: "no extra args uses cwd config.yaml",
			args: []string{"/usr/bin/status-check"},
			want: "config.yaml",
		},
		{
			name: "single arg is config path",
			args: []string{"/app/status-check", "/etc/status-check/prod.yaml"},
			want: "/etc/status-check/prod.yaml",
		},
		{
			name:    "two extra args is an error",
			args:    []string{"status-check", "a.yaml", "b.yaml"},
			wantErr: true,
		},
		{
			name:    "many extra args is an error",
			args:    []string{"status-check", "x", "y", "z"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := configPathFromArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), "usage:") {
					t.Errorf("error should mention usage, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("configPathFromArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}
