package bridge

import (
	"strings"
	"testing"
)

func TestChunkText_Short(t *testing.T) {
	got := chunkText("hello", 100)
	if len(got) != 1 || got[0] != "hello" {
		t.Fatalf("want [hello], got %#v", got)
	}
}

func TestChunkText_SplitOnParagraph(t *testing.T) {
	s := strings.Repeat("a", 30) + "\n\n" + strings.Repeat("b", 30)
	got := chunkText(s, 40)
	if len(got) != 2 {
		t.Fatalf("want 2 chunks, got %d: %#v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "aaa") || !strings.HasPrefix(got[1], "bbb") {
		t.Fatalf("unexpected split: %#v", got)
	}
}

func TestChunkText_HardSplitWhenNoBoundary(t *testing.T) {
	s := strings.Repeat("x", 250)
	got := chunkText(s, 100)
	if len(got) != 3 {
		t.Fatalf("want 3 chunks, got %d", len(got))
	}
	if len(got[0]) > 100 || len(got[1]) > 100 {
		t.Fatalf("chunks exceed max: %d %d", len(got[0]), len(got[1]))
	}
}

func TestSplitIRCLines_NewlinesAndWrapping(t *testing.T) {
	s := "first line\n" + strings.Repeat("word ", 100) + "\nlast"
	got := splitIRCLines(s, 80)
	if len(got) < 3 {
		t.Fatalf("want >=3 lines, got %d", len(got))
	}
	if got[0] != "first line" {
		t.Fatalf("first line: %q", got[0])
	}
	if got[len(got)-1] != "last" {
		t.Fatalf("last line: %q", got[len(got)-1])
	}
	for _, line := range got {
		if len(line) > 80 {
			t.Fatalf("line exceeds 80 bytes: %q", line)
		}
	}
}

func TestSplitIRCLines_StripsBlanks(t *testing.T) {
	got := splitIRCLines("a\n\n\nb\n", 80)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("want [a b], got %#v", got)
	}
}
