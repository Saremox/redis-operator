package prober

import (
	"testing"
	"time"
)

func TestOutage(t *testing.T) {
	var o outage
	t0 := time.Unix(1000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

	if _, ended := o.succeed(at(0)); ended {
		t.Fatal("success without an outage ended one")
	}
	if !o.fail(at(1)) {
		t.Fatal("first failure didn't start an outage")
	}
	if o.fail(at(2)) || o.fail(at(3)) {
		t.Fatal("later failures started another outage")
	}
	d, ended := o.succeed(at(4))
	if !ended || d != 3*time.Second {
		t.Fatalf("got %v, %v; want 3s, true", d, ended)
	}
	if _, ended := o.succeed(at(5)); ended {
		t.Fatal("second success ended an outage")
	}
	if !o.fail(at(6)) {
		t.Fatal("a new failure didn't start a new outage")
	}
	if d, _ := o.succeed(at(8)); d != 2*time.Second {
		t.Fatalf("got %v, want 2s", d)
	}
}
