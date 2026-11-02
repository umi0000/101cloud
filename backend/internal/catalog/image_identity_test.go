package catalog

import (
	"strings"
	"testing"
)

func TestImageIdentityDefaultsWhenCodeAndVersionOmitted(t *testing.T) {
	fingerprint := "4FAAA23407EC789d992909cbbcd600d34135dd6aa8e43198720eac67fef75268"
	base := imageInput{InstanceType: "container", SourceType: "local", Nodes: []ImageNodeInput{{NodeID: "1", ProjectName: "default", Architecture: "x86_64", Fingerprint: fingerprint}}, Properties: map[string]any{"gpu_ready": false, "os_name": "ubuntu", "os_release": "22.04"}}

	row, err := imageValues(base, 1)
	if err != nil {
		t.Fatal(err)
	}
	if row["code"] != "4faaa23407ec" || row["version"] != "1" || row["name"] != "ubuntu 22.04 (4faaa23407ec)" {
		t.Fatalf("defaults: code=%v version=%v name=%v", row["code"], row["version"], row["name"])
	}

	explicit := base
	explicit.Code, explicit.Version = "jammy", "2026.10"
	if row, err = imageValues(explicit, 1); err != nil || row["name"] != "jammy 2026.10" {
		t.Fatalf("explicit identity: %v %v", row["name"], err)
	}

	versionOnly := base
	versionOnly.Version = "7"
	if row, err = imageValues(versionOnly, 1); err != nil || row["code"] != "4faaa23407ec" || row["version"] != "7" {
		t.Fatalf("version only: %v %v %v", row["code"], row["version"], err)
	}

	long := base
	long.Properties = map[string]any{"gpu_ready": false, "os_name": strings.Repeat("u", 64), "os_release": strings.Repeat("r", 64)}
	if row, err = imageValues(long, 1); err != nil || row["name"] != "4faaa23407ec 1" {
		t.Fatalf("over-long default label must fall back within column size: %v %v", row["name"], err)
	}

	for _, bad := range []imageInput{{Code: "-x"}, {Version: "bad version"}} {
		in := base
		in.Code, in.Version = bad.Code, bad.Version
		if _, err = imageValues(in, 1); err == nil {
			t.Fatalf("invalid explicit identity accepted: %+v", bad)
		}
	}
}
