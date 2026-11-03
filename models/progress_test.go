package models

import (
	"context"
	"testing"
)

func TestProgress(t *testing.T) {
	e := Registry[0]
	ReportProgress(context.Background(), e, 1, 2) // no function: nothing
	var got [][2]int64
	ctx := WithProgress(context.Background(), func(g Entry, d, n int64) {
		if g.Name != e.Name {
			t.Errorf("entry %s", g.Name)
		}
		got = append(got, [2]int64{d, n})
	})
	ReportProgress(ctx, e, 5, 10)
	ReportProgress(ctx, e, 10, 10)
	if len(got) != 2 || got[0] != [2]int64{5, 10} || got[1] != [2]int64{10, 10} {
		t.Fatalf("%v", got)
	}
}
