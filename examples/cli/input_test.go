package main

import "testing"

func TestModifiedEnter(t *testing.T) {
	tests := []struct {
		seq      string
		modifier string
		ok       bool
	}{
		{"\x1b[13;2u", "2", true},
		{"\x1b[13;5u", "5", true},
		{"\x1b[13;9u", "9", true},
		{"\x1b[27;2;13~", "2", true},
		{"\x1b[A", "", false},
	}

	for _, tt := range tests {
		modifier, ok := modifiedEnter([]byte(tt.seq))
		if modifier != tt.modifier || ok != tt.ok {
			t.Errorf("modifiedEnter(%q) = (%q, %v), want (%q, %v)", tt.seq, modifier, ok, tt.modifier, tt.ok)
		}
	}
}
