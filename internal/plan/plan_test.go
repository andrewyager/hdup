package plan

import "testing"

const mib = 1 << 20

func TestPartsOfEmptyFileIsEmpty(t *testing.T) {
	if got := Parts(0, 64*mib); len(got) != 0 {
		t.Fatalf("got %d parts, want 0", len(got))
	}
}

func TestPartsOfFileSmallerThanOnePartIsOnePart(t *testing.T) {
	got := Parts(10, 64*mib)
	want := []Part{{Number: 1, Offset: 0, Size: 10}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPartsOfExactMultipleHasNoShortPart(t *testing.T) {
	got := Parts(8, 4)
	if len(got) != 2 || got[1] != (Part{Number: 2, Offset: 4, Size: 4}) {
		t.Fatalf("got %v", got)
	}
}

func TestPartsLastPartHoldsTheRemainder(t *testing.T) {
	got := Parts(14, 4)
	if len(got) != 4 || got[3] != (Part{Number: 4, Offset: 12, Size: 2}) {
		t.Fatalf("got %v", got)
	}
}

func TestPartSizeDoublesUntilFileFitsInMaxParts(t *testing.T) {
	if got := PartSize(64*mib*MaxParts, 64*mib); got != 64*mib {
		t.Fatalf("at the limit: got %d", got)
	}
	if got := PartSize(64*mib*MaxParts+1, 64*mib); got != 128*mib {
		t.Fatalf("above the limit: got %d", got)
	}
}

func TestPartsCoverTheFileWithNoGaps(t *testing.T) {
	const size = 1000003
	var next int64
	for i, p := range Parts(size, 4096) {
		if p.Number != int32(i+1) || p.Offset != next {
			t.Fatalf("part %d: %+v, want offset %d", i, p, next)
		}
		next += p.Size
	}
	if next != size {
		t.Fatalf("parts cover %d bytes, want %d", next, size)
	}
}
