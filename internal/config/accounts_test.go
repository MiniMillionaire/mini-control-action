package config

import (
	"strings"
	"testing"
)

const multiConfig = `
github:
  accounts:
    personal:
      token: test-personal
      owners: [example]
      exclude: [example/ignored]
      defaults: {pool: standard, max_concurrent_vms: 2}
      repositories:
        example/release: {pool: release, max_concurrent_vms: 1}
    company:
      token: test-company
      defaults: {pool: standard}
minicontrol:
  base_url: https://example.test/api/client/v1
  api_key: mck_test
pools:
  standard:
    sku: m4-big-v1
    base_image: standard-image
    region: east
    max_concurrent_vms: 4
  release:
    sku: m4-big-v1
    base_image: release-image
    region: west
    max_concurrent_vms: 2
`

func TestAccountPolicyInheritance(t *testing.T) {
	cfg, err := Load(writeConfig(t, multiConfig))
	if err != nil {
		t.Fatal(err)
	}
	plan, limit, paused, err := cfg.Resolve("personal", "example/new")
	if err != nil || paused || limit != 2 || plan.Pool != "standard" || plan.Region != "east" || plan.BaseImage != "standard-image" {
		t.Fatalf("unexpected defaults: %+v %d %v %v", plan, limit, paused, err)
	}
	plan, limit, _, err = cfg.Resolve("personal", "EXAMPLE/RELEASE")
	if err != nil || plan.Pool != "release" || limit != 1 || plan.Region != "west" || plan.BaseImage != "release-image" {
		t.Fatalf("override not applied: %+v %d %v", plan, limit, err)
	}
	for _, repo := range []string{"example/ignored", "other/repo"} {
		_, _, paused, err = cfg.Resolve("personal", repo)
		if err != nil || !paused {
			t.Fatalf("excluded repository %s was allowed", repo)
		}
	}
}

func TestAccountPolicyRejectsAmbiguousOrWiderSettings(t *testing.T) {
	for name, body := range map[string]string{
		"unknown pool":         strings.Replace(multiConfig, "pool: release", "pool: missing", 1),
		"missing region":       strings.Replace(multiConfig, "    region: east\n", "", 1),
		"padded region":        strings.Replace(multiConfig, "region: east", "region: \" east\"", 1),
		"region list":          strings.Replace(multiConfig, "region: east", "region: \"east,west\"", 1),
		"missing image":        strings.Replace(multiConfig, "    base_image: standard-image\n", "", 1),
		"invalid image":        strings.Replace(multiConfig, "base_image: standard-image", "base_image: \"bad image\"", 1),
		"zero limit":           strings.Replace(multiConfig, "max_concurrent_vms: 1", "max_concurrent_vms: 0", 1),
		"empty token":          strings.Replace(multiConfig, "test-company", "\"\"", 1),
		"duplicate repository": strings.Replace(multiConfig, "token: test-company", "token: test-company\n      repositories:\n        EXAMPLE/RELEASE: {}", 1),
		"unknown field":        strings.Replace(multiConfig, "token: test-company", "token: test-company\n      typo: true", 1),
		// The removed fallbacks are unknown fields now, not silently ignored.
		"regions fallback":   strings.Replace(multiConfig, "region: east", "regions: [east, west]", 1),
		"any region":         strings.Replace(multiConfig, "region: east", "any_region: true", 1),
		"image fallback":     strings.Replace(multiConfig, "base_image: standard-image", "base_images: [standard-image, other]", 1),
		"repository regions": strings.Replace(multiConfig, "pool: release,", "pool: release, regions: [west],", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
