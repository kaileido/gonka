package loadtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"devshard/testenv/config"

	"gopkg.in/yaml.v3"
)

const (
	buildFileName     = "build.json"
	testenvBuildTag   = "devshard_testenv"
	testenvEnvPrefix  = "DEVSHARD_TESTENV_"
	tripfileEventKind = "tripfile"
)

// Build names prebuilt artifacts under test. The stack runs them instead of
// building devshardd, versiond, versiond-router and the gateway from the checkout.
type Build struct {
	Name          string      `yaml:"name" json:"name"`
	Protocol      string      `yaml:"protocol" json:"protocol"`
	BinaryVersion string      `yaml:"binary_version" json:"binary_version"`
	Devshardd     string      `yaml:"devshardd" json:"devshardd"`
	Images        BuildImages `yaml:"images" json:"images"`
	// Versions are further devshardd versions every host runs next to Protocol.
	Versions []BuildVersion `yaml:"versions" json:"versions,omitempty"`
	Gateway  BuildGateway   `yaml:"gateway" json:"gateway"`
	// PeerRPC sets the peer transport of the gateway and the hosts; unset keeps the harness's native gRPC.
	PeerRPC BuildPeerRPC `yaml:"peer_rpc" json:"peer_rpc"`
}

type BuildPeerRPC struct {
	GRPC      *bool `yaml:"grpc" json:"grpc,omitempty"`
	H2Upgrade *bool `yaml:"h2_upgrade" json:"h2_upgrade,omitempty"`
}

type BuildVersion struct {
	Name      string `yaml:"name" json:"name"`
	Devshardd string `yaml:"devshardd" json:"devshardd"`
}

type BuildGateway struct {
	// Env adds gateway env, e.g. a production gateway's settings.
	Env map[string]string `yaml:"env" json:"env,omitempty"`
}

type BuildImages struct {
	Versiond       string `yaml:"versiond" json:"versiond"`
	VersiondRouter string `yaml:"versiond_router" json:"versiond_router"`
	Gateway        string `yaml:"gateway" json:"gateway"`
}

// BuildRecord is build.json: what the run actually ran, by content.
type BuildRecord struct {
	Build
	DevsharddSHA256 string            `json:"devshardd_sha256"`
	DevsharddTags   string            `json:"devshardd_tags"`
	VersionsSHA256  map[string]string `json:"versions_sha256,omitempty"`
	ImageIDs        map[string]string `json:"image_ids"`
}

// LoadBuild reads a build file; a relative devshardd path is relative to the file.
func LoadBuild(path string) (*Build, error) {
	var build Build
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	// Strict: an unknown key is a typo or a field this checkout lacks, never a silent no-op.
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	decoder.KnownFields(true)
	if err := decoder.Decode(&build); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
	build.Devshardd = resolve(build.Devshardd)
	for i, version := range build.Versions {
		if strings.TrimSpace(version.Name) == "" || strings.TrimSpace(version.Devshardd) == "" || version.Name == build.Protocol {
			return nil, fmt.Errorf("build %s: versions[%d] needs a name other than protocol and a devshardd", path, i)
		}
		build.Versions[i].Devshardd = resolve(version.Devshardd)
	}
	for _, field := range []struct{ name, value string }{
		{"name", build.Name},
		{"protocol", build.Protocol},
		{"binary_version", build.BinaryVersion},
		{"devshardd", build.Devshardd},
		{"images.versiond", build.Images.Versiond},
		{"images.versiond_router", build.Images.VersiondRouter},
		{"images.gateway", build.Images.Gateway},
	} {
		if strings.TrimSpace(field.value) == "" {
			return nil, fmt.Errorf("build %s: %s is required", path, field.name)
		}
	}
	return &build, nil
}

func (b *Build) apply(cfg *config.File) {
	cfg.Versiond.VersionName = b.Protocol
	cfg.Versiond.BinaryVersion = b.BinaryVersion
	cfg.Versiond.HostBinaryMount = b.Devshardd
	cfg.Versiond.Image = b.Images.Versiond
	cfg.VersiondRouter.Image = b.Images.VersiondRouter
	cfg.Devshardctl.Image = b.Images.Gateway
	cfg.MockDapi.AnnounceVersion = true
	cfg.Versiond.ExtraVersions = nil
	for _, version := range b.Versions {
		cfg.Versiond.ExtraVersions = append(cfg.Versiond.ExtraVersions, config.ExtraVersion{Name: version.Name, HostBinaryMount: version.Devshardd})
	}
	cfg.Devshardctl.Env = b.Gateway.Env
}

// recordBuild checks that every image is present locally, so Compose never
// pulls or builds a substitute, and writes build.json with content ids.
func recordBuild(ctx context.Context, build *Build, outputDir string) error {
	record := BuildRecord{Build: *build, ImageIDs: map[string]string{}}
	for _, image := range []string{build.Images.Versiond, build.Images.VersiondRouter, build.Images.Gateway} {
		output, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image).Output()
		if err != nil {
			return fmt.Errorf("build %s: image %s is not loaded: %w", build.Name, image, err)
		}
		record.ImageIDs[image] = strings.TrimSpace(string(output))
	}
	sum, err := fileSHA256(build.Devshardd)
	if err != nil {
		return fmt.Errorf("build %s: %w", build.Name, err)
	}
	record.DevsharddSHA256 = sum
	if record.DevsharddTags, err = devsharddBuildTags(build.Devshardd); err != nil {
		return fmt.Errorf("build %s: %w", build.Name, err)
	}
	for _, version := range build.Versions {
		if record.VersionsSHA256 == nil {
			record.VersionsSHA256 = map[string]string{}
		}
		if record.VersionsSHA256[version.Name], err = fileSHA256(version.Devshardd); err != nil {
			return fmt.Errorf("build %s: %w", build.Name, err)
		}
	}
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outputDir, buildFileName), append(body, '\n'), 0o644)
}

// requireTestenvHooks refuses a scenario whose faults live inside devshardd
// when the binary lacks them: such a fault would silently not happen.
func requireTestenvHooks(scenario Scenario, devshardd string) error {
	var uses []string
	for _, event := range scenario.TimelineEvents() {
		if event.Action == tripfileEventKind {
			uses = append(uses, "event "+event.ID)
		}
	}
	for _, host := range scenario.Topology.Hosts {
		for key, value := range host.ResolvedEnv() {
			if strings.HasPrefix(key, testenvEnvPrefix) && value != "" {
				uses = append(uses, host.ID+" "+key)
			}
		}
	}
	if len(uses) == 0 {
		return nil
	}
	tags, err := devsharddBuildTags(devshardd)
	if err != nil {
		return err
	}
	if slices.Contains(strings.Split(tags, ","), testenvBuildTag) {
		return nil
	}
	slices.Sort(uses)
	return fmt.Errorf("%s needs devshardd built with -tags %s, but %s has tags %q", strings.Join(uses, ", "), testenvBuildTag, devshardd, tags)
}

func devsharddBuildTags(path string) (string, error) {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read build info of %s: %w", path, err)
	}
	for _, setting := range info.Settings {
		if setting.Key == "-tags" {
			return setting.Value, nil
		}
	}
	return "", nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
