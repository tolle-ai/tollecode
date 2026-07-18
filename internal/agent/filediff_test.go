package agent

import "testing"

func lineKinds(hunks []map[string]any) []string {
	var kinds []string
	for _, h := range hunks {
		for _, ln := range h["lines"].([]map[string]any) {
			kinds = append(kinds, ln["kind"].(string))
		}
	}
	return kinds
}

func TestComputeLineDiff_NoChange(t *testing.T) {
	hunks, add, del := computeLineDiff("a\nb\nc\n", "a\nb\nc\n")
	if len(hunks) != 0 || add != 0 || del != 0 {
		t.Fatalf("expected no diff, got hunks=%d add=%d del=%d", len(hunks), add, del)
	}
}

func TestComputeLineDiff_NewFile(t *testing.T) {
	hunks, add, del := computeLineDiff("", "one\ntwo\n")
	if add != 2 || del != 0 {
		t.Fatalf("expected +2 -0, got +%d -%d", add, del)
	}
	if len(hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(hunks))
	}
	first := hunks[0]["lines"].([]map[string]any)[0]
	if first["newNo"].(int) != 1 || first["text"].(string) != "one" {
		t.Fatalf("unexpected first added line: %+v", first)
	}
}

func TestComputeLineDiff_SingleLineReplace(t *testing.T) {
	old := "package main\n\nfunc foo() {\n\treturn bar\n}\n"
	neu := "package main\n\nfunc foo() {\n\tx := compute()\n\treturn x\n}\n"
	hunks, add, del := computeLineDiff(old, neu)
	if add != 2 || del != 1 {
		t.Fatalf("expected +2 -1, got +%d -%d", add, del)
	}
	if len(hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(hunks))
	}
	kinds := lineKinds(hunks)
	// The removed line keeps its old line number; the change is surrounded by context.
	var gotDel, gotAdd, gotCtx int
	for _, k := range kinds {
		switch k {
		case "del":
			gotDel++
		case "add":
			gotAdd++
		case "context":
			gotCtx++
		}
	}
	if gotDel != 1 || gotAdd != 2 || gotCtx == 0 {
		t.Fatalf("unexpected kinds del=%d add=%d ctx=%d", gotDel, gotAdd, gotCtx)
	}
}

func TestComputeLineDiff_SeparateHunks(t *testing.T) {
	// Two changes far apart should collapse the unchanged middle into two hunks.
	var oldSB, newSB string
	for i := 0; i < 40; i++ {
		oldSB += "line\n"
		newSB += "line\n"
	}
	old := "HEAD\n" + oldSB + "TAIL\n"
	neu := "HEADX\n" + newSB + "TAILX\n"
	hunks, add, del := computeLineDiff(old, neu)
	if add != 2 || del != 2 {
		t.Fatalf("expected +2 -2, got +%d -%d", add, del)
	}
	if len(hunks) != 2 {
		t.Fatalf("expected 2 hunks (collapsed middle), got %d", len(hunks))
	}
}

func TestComputeLineDiff_LargeDivergentFallback(t *testing.T) {
	// Force the LCS fallback path and confirm it still tallies every line.
	var old, neu string
	for i := 0; i < 2100; i++ {
		old += "a\n"
		neu += "b\n"
	}
	hunks, add, del := computeLineDiff(old, neu)
	if add != 2100 || del != 2100 {
		t.Fatalf("expected +2100 -2100, got +%d -%d", add, del)
	}
	if len(hunks) == 0 {
		t.Fatal("expected at least one hunk")
	}
}
