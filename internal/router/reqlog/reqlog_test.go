package reqlog

import "testing"

func TestNopSink_LogClose(t *testing.T) {
	var s NopSink
	s.Log(Record{Model: "x", Status: 200}) // must not panic
	if err := s.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
}

func TestMemorySink_RecordsInOrder(t *testing.T) {
	var s MemorySink
	for _, model := range []string{"a", "b", "c"} {
		s.Log(Record{Model: model})
	}
	if got := s.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}
	recs := s.Records()
	for i, want := range []string{"a", "b", "c"} {
		if recs[i].Model != want {
			t.Errorf("rec[%d].Model = %q, want %q", i, recs[i].Model, want)
		}
	}
	// Records() must return a copy: mutating it must not affect future reads.
	recs[0].Model = "mutated"
	if again := s.Records(); again[0].Model != "a" {
		t.Errorf("Records() did not return a copy: a=%q", again[0].Model)
	}
}

func TestRedactDSN(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"postgresql://user:secret@host:5432/db", "postgresql://user:***@host:5432/db"},
		{"postgres://u:p@127.0.0.1/x?sslmode=disable", "postgres://u:***@127.0.0.1/x?sslmode=disable"},
		{"postgresql://host:5432/db", "postgresql://host:5432/db"},               // no userinfo
		{"host=localhost user=u password=p", "host=localhost user=u password=p"}, // kv form passes through
		{"", ""},
	}
	for _, tc := range tests {
		if got := RedactDSN(tc.in); got != tc.want {
			t.Errorf("RedactDSN(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
