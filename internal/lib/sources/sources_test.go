package sources

import (
	"context"
	"testing"
)

func TestSourcesRoundTrip(t *testing.T) {
	want := Sources{Resource: "/Stage[main]/Main/Exec[x]", Provider: "Exec[x](provider=posix)", Ref: "Exec[x]"}
	if got := SourcesFrom(WithSources(context.Background(), want)); got != want {
		t.Errorf("SourcesFrom = %+v, want %+v", got, want)
	}
}

func TestSourcesFromBareContextIsZero(t *testing.T) {
	if got := SourcesFrom(context.Background()); got != (Sources{}) {
		t.Errorf("SourcesFrom = %+v, want the zero Sources", got)
	}
}
