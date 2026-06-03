package provider

import "testing"

func TestManagedRemarkRoundTrip(t *testing.T) {
	cases := []ManagedRemark{
		{Instance: "", Source: "x.eo.dnse1.com"},
		{Instance: "beijing", Source: "x.eo.dnse1.com"},
		{Instance: "shanghai", Source: "x.eo.dnse1.com"},
	}
	for _, c := range cases {
		got, ok := ParseManagedRemark(BuildManagedRemark(c))
		if !ok || got != c {
			t.Fatalf("round-trip failed for %+v: got %+v ok=%v", c, got, ok)
		}
	}
}

func TestManagedRemarkInstanceIsolation(t *testing.T) {
	bj := BuildManagedRemark(ManagedRemark{Instance: "beijing", Source: "x.eo.dnse1.com"})
	m, ok := ParseManagedRemark(bj)
	if !ok {
		t.Fatal("expected managed")
	}
	// A shanghai worker must not recognise a beijing record as its own.
	if m.Instance == "shanghai" {
		t.Fatalf("instance isolation broken: %+v", m)
	}
}

func TestParseRejectsUserRemark(t *testing.T) {
	if _, ok := ParseManagedRemark("my own record"); ok {
		t.Fatal("user remark must not be claimed")
	}
	if _, ok := ParseManagedRemark("flatns-managed:i=onlyinstance"); ok {
		t.Fatal("malformed named-instance remark must be rejected")
	}
}
