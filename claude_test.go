package main

import (
	"fmt"
	"reflect"
	"testing"
)

func TestParseCommands(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantCleanText string
		wantCommands  []string
	}{
		{
			name:          "No commands",
			input:         "Hello world",
			wantCleanText: "Hello world",
			wantCommands:  nil,
		},
		{
			name:          "Single command",
			input:         "Run <command>ls -la</command> please",
			wantCleanText: "Run `ls -la` please",
			wantCommands:  []string{"ls -la"},
		},
		{
			name:          "Multiple commands",
			input:         "First <command>echo 1</command> then <command>echo 2</command>",
			wantCleanText: "First `echo 1` then `echo 2`",
			wantCommands:  []string{"echo 1", "echo 2"},
		},
		{
			name:          "Multiline command",
			input:         "Run:\n<command>\nls -la\ngrep foo\n</command>",
			wantCleanText: "Run:\n`ls -la\ngrep foo`",
			wantCommands:  []string{"ls -la\ngrep foo"},
		},
		{
			name:          "Whitespace trimming",
			input:         "<command>  ls  </command>",
			wantCleanText: "`ls`",
			wantCommands:  []string{"ls"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCleanText, gotCommands := ParseCommands(tt.input)
			if gotCleanText != tt.wantCleanText {
				t.Errorf("ParseCommands() cleanText = %q, want %q", gotCleanText, tt.wantCleanText)
			}
			if !reflect.DeepEqual(gotCommands, tt.wantCommands) {
				t.Errorf("ParseCommands() commands = %v, want %v", gotCommands, tt.wantCommands)
			}
		})
	}
}

func TestIsNotLoggedIn(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"Nil error", nil, false},
		{"Generic error", fmt.Errorf("some random error"), false},
		{"Exact match", fmt.Errorf("Not logged in"), true},
		{"Lowercase match", fmt.Errorf("not logged in"), true},
		{"Wrapped match", fmt.Errorf("error: Not logged in to Claude"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsNotLoggedIn(tt.err); got != tt.want {
				t.Errorf("IsNotLoggedIn() = %v, want %v", got, tt.want)
			}
		})
	}
}
