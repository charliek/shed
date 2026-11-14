package config

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/charliek/shed/internal/remote/provider"
)

func TestNewShedID(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		id, err := NewShedID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != ShedIDLength || !ValidShedID(id) {
			t.Fatalf("ID %q is not %d lowercase base32 characters", id, ShedIDLength)
		}
		if !provider.ValidShedID(id) || !provider.ValidLabelValue(id) {
			t.Fatalf("ID %q is not a legal shed_id label value", id)
		}
		if seen[id] {
			t.Fatalf("ID %q minted twice", id)
		}
		seen[id] = true
	}
}

func TestNewShedIDEncoding(t *testing.T) {
	tests := []struct {
		name    string
		src     io.Reader
		want    string
		wantErr bool
	}{
		{"zero bytes", bytes.NewReader(make([]byte, 16)), "aaaaaaaaaaaaaaaaaaaaaaaaaa", false},
		{"all ones", bytes.NewReader(bytes.Repeat([]byte{0xff}, 16)), "77777777777777777777777774", false},
		{"short read", bytes.NewReader(make([]byte, 15)), "", true},
		{"failing source", errReader{}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := newShedID(tt.src)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("got %q, want an error", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ID = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

func TestValidShedID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"aaaaaaaaaaaaaaaaaaaaaaaaaa", true},
		{"abcdefghijklmnopqrstuvwxyz", true},
		{"a2b3c4d5e6f7a2b3c4d5e6f7a2", true},
		{"", false},
		{"aaaaaaaaaaaaaaaaaaaaaaaaa", false},   // 25
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaa", false}, // 27
		{"AAAAAAAAAAAAAAAAAAAAAAAAAA", false},
		{"aaaaaaaaaaaaaaaaaaaaaaaaa1", false}, // 1 is not in the alphabet
		{"aaaaaaaaaaaaa-aaaaaaaaaaaa", false},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			if got := ValidShedID(tt.id); got != tt.want {
				t.Errorf("ValidShedID(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}
