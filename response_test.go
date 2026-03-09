package main

import "testing"

func TestSegmentAccumulatorSplitsByRunes(t *testing.T) {
	t.Parallel()

	accumulator := newSegmentAccumulator(4)
	splitOccurred := accumulator.appendText("abecd")

	if !splitOccurred {
		t.Fatal("expected content to split across segments")
	}

	segments := accumulator.renderSegments(true)
	if len(segments) != 2 {
		t.Fatalf("unexpected segment count: %#v", segments)
	}

	if segments[0] != "abec" || segments[1] != "d" {
		t.Fatalf("unexpected segments: %#v", segments)
	}
}

func TestBuildRenderSpecsMarksSettledAndStreamingSegments(t *testing.T) {
	t.Parallel()

	specs := buildRenderSpecs([]string{"first", "second"}, "length", false)
	if len(specs) != 2 {
		t.Fatalf("unexpected spec count: %#v", specs)
	}

	if specs[0].content != "first" || specs[0].color != embedColorComplete {
		t.Fatalf("unexpected first spec: %#v", specs[0])
	}

	if specs[1].content != "second"+streamingIndicator ||
		specs[1].color != embedColorIncomplete {
		t.Fatalf("unexpected second spec: %#v", specs[1])
	}

	finalSpecs := buildRenderSpecs([]string{"only"}, "stop", true)
	if len(finalSpecs) != 1 {
		t.Fatalf("unexpected final spec count: %#v", finalSpecs)
	}

	if finalSpecs[0].content != "only" || finalSpecs[0].color != embedColorComplete {
		t.Fatalf("unexpected final spec: %#v", finalSpecs[0])
	}
}
