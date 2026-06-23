package config

import "testing"

func TestDaemonRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := Daemon{Hub: "hub.shoemoney.ai", Mounts: []Mount{
		{Share: "projects", Local: "/Users/jh/Projects", Hub: "hub.shoemoney.ai"},
		{Share: "projects", Subpath: "p22/backend", Local: "/var/www", Hub: "hub.shoemoney.ai", ReadOnly: true},
	}}
	if err := SaveDaemon(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadDaemon(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out.Hub != in.Hub || len(out.Mounts) != 2 {
		t.Fatalf("roundtrip mismatch: %+v", out)
	}
	if !out.Mounts[1].ReadOnly || out.Mounts[1].Subpath != "p22/backend" {
		t.Fatalf("mount fields lost: %+v", out.Mounts[1])
	}
}

func TestLoadDaemonMissingIsEmpty(t *testing.T) {
	d, err := LoadDaemon(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if d.Hub != "" || len(d.Mounts) != 0 {
		t.Fatalf("expected empty daemon for missing file, got %+v", d)
	}
}
