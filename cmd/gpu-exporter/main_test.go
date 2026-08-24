package main

import "testing"

const sampleFdinfo = `pos:	0
flags:	02100002
drm-driver:	xe
drm-client-id:	48
drm-pdev:	0000:05:00.0
drm-resident-vram0:	32677152 KiB
drm-cycles-rcs:	3021
drm-total-cycles-rcs:	2013619918356
drm-cycles-ccs:	4501008337
drm-total-cycles-ccs:	2013619918356
drm-cycles-bcs:	5058286
drm-total-cycles-bcs:	2013619940371
`

func TestParseFdinfo(t *testing.T) {
	c := parseFdinfo(sampleFdinfo)
	if c == nil {
		t.Fatal("parseFdinfo returned nil for a DRM client")
	}
	if c.pdev != "0000:05:00.0" || c.clientID != "48" {
		t.Errorf("pdev/client = %q/%q", c.pdev, c.clientID)
	}
	if c.residentKiB != 32677152 {
		t.Errorf("residentKiB = %d", c.residentKiB)
	}
	if c.cycles["ccs"] != 4501008337 {
		t.Errorf("ccs cycles = %d", c.cycles["ccs"])
	}
	if c.totalCycles["bcs"] != 2013619940371 {
		t.Errorf("bcs total = %d", c.totalCycles["bcs"])
	}
}

func TestParseFdinfo_NonDRMFdIsNil(t *testing.T) {
	if c := parseFdinfo("pos:\t0\nflags:\t02\n"); c != nil {
		t.Errorf("non-DRM fdinfo parsed as client: %+v", c)
	}
}

func TestBusyPct(t *testing.T) {
	prev := cardSample{
		cycles:      map[string]uint64{"ccs": 1000, "rcs": 0},
		totalCycles: map[string]uint64{"ccs": 10000, "rcs": 10000},
	}
	cur := cardSample{
		cycles:      map[string]uint64{"ccs": 1600, "rcs": 5},
		totalCycles: map[string]uint64{"ccs": 11000, "rcs": 11000},
	}
	// ccs: 600/1000 = 60%; rcs: 5/1000 = 0%. Busiest class wins.
	if b := busyPct(cur, prev); b == nil || *b != 60 {
		t.Errorf("busyPct = %v, want 60", b)
	}
}

func TestBusyPct_FirstScanIsNil(t *testing.T) {
	cur := cardSample{
		cycles:      map[string]uint64{"ccs": 100},
		totalCycles: map[string]uint64{"ccs": 1000},
	}
	if b := busyPct(cur, cardSample{}); b != nil {
		t.Errorf("first scan busy = %v, want nil", *b)
	}
}

func TestBusyPct_ClientChurnSkipsClass(t *testing.T) {
	prev := cardSample{
		cycles:      map[string]uint64{"ccs": 5000},
		totalCycles: map[string]uint64{"ccs": 10000},
	}
	// Summed cycles went backwards (a client exited): class skipped, nil.
	cur := cardSample{
		cycles:      map[string]uint64{"ccs": 100},
		totalCycles: map[string]uint64{"ccs": 11000},
	}
	if b := busyPct(cur, prev); b != nil {
		t.Errorf("churned class busy = %v, want nil", *b)
	}
}
