package main

import (
	"slices"
	"testing"
)

func TestCompleteWords(t *testing.T) {
	// A home that holds no config keeps the repo-derived sources empty, so only
	// the static command tree is exercised here.
	t.Setenv("KOMPENSATOR_HOME", t.TempDir())

	tests := []struct {
		name string
		args []string
		want []string
		miss []string
	}{
		{
			name: "top level commands",
			args: []string{""},
			want: []string{"reconcile", "status", "secrets", "completion"},
			miss: []string{"__complete", "bootstrap"},
		},
		{
			name: "command prefix",
			args: []string{"st"},
			want: []string{"stack", "status"},
			miss: []string{"secrets"},
		},
		{
			name: "subcommands",
			args: []string{"secrets", ""},
			want: []string{"set", "set-key", "set-file", "show", "edit", "rekey"},
		},
		{
			name: "nested subcommands",
			args: []string{"controller", "repo", ""},
			want: []string{"add"},
		},
		{
			name: "env subcommands",
			args: []string{"env", ""},
			want: []string{"list", "add", "remove", "stack"},
		},
		{
			name: "env stack subcommands",
			args: []string{"env", "stack", ""},
			want: []string{"add", "remove"},
		},
		{
			name: "state subcommands",
			args: []string{"state", ""},
			want: []string{"set"},
		},
		{
			name: "flags with two dashes",
			args: []string{"reconcile", "--"},
			want: []string{"--force", "--prune", "--ignore-pause", "--repo", "--home", "--json"},
		},
		{
			name: "flags with one dash",
			args: []string{"reconcile", "-f"},
			want: []string{"-force"},
			miss: []string{"-prune"},
		},
		{
			name: "subcommand flags only on the subcommand",
			args: []string{"node", "remove", "--k"},
			want: []string{"--keep-containers", "--keep-home"},
			miss: []string{"--schedule"},
		},
		{
			name: "static flag values",
			args: []string{"project", "add", "--strategy", ""},
			want: []string{"blue-green", "recreate"},
		},
		{
			name: "shells",
			args: []string{"completion", ""},
			want: []string{"bash", "zsh", "fish"},
		},
		{
			name: "global flag before the command",
			args: []string{"-home", "/tmp", "st"},
			want: []string{"stack", "status"},
		},
		{
			name: "no subcommands once a positional is taken",
			args: []string{"completion", "bash", ""},
			miss: []string{"bash", "version"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := completeWords(tc.args)
			for _, w := range tc.want {
				if !slices.Contains(got, w) {
					t.Errorf("missing candidate %q in %v", w, got)
				}
			}
			for _, w := range tc.miss {
				if slices.Contains(got, w) {
					t.Errorf("unexpected candidate %q in %v", w, got)
				}
			}
		})
	}
}

func TestCompletionScript(t *testing.T) {
	for _, shell := range compShells {
		script, ok := completionScript(shell, "kompensator")
		if !ok || script == "" {
			t.Fatalf("no script for %s", shell)
		}
	}
	if _, ok := completionScript("tcsh", "kompensator"); ok {
		t.Error("tcsh should not be supported")
	}
}
