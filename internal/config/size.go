package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Size is a byte size that accepts suffixes such as 8G or 512M. Suffixes are
// binary (1K = 1024).
type Size int64

// Bytes returns the size in bytes.
func (s Size) Bytes() int64 { return int64(s) }

// ParseSize parses a human readable size. A bare number is treated as bytes.
func ParseSize(text string) (Size, error) {
	value := strings.ToUpper(strings.TrimSpace(text))
	if value == "" {
		return 0, errors.New("empty size")
	}
	suffixes := []struct {
		suffix string
		mult   int64
	}{
		{"KIB", 1 << 10}, {"KB", 1 << 10}, {"K", 1 << 10},
		{"MIB", 1 << 20}, {"MB", 1 << 20}, {"M", 1 << 20},
		{"GIB", 1 << 30}, {"GB", 1 << 30}, {"G", 1 << 30},
		{"TIB", 1 << 40}, {"TB", 1 << 40}, {"T", 1 << 40},
	}
	mult := int64(1)
	for _, s := range suffixes {
		if strings.HasSuffix(value, s.suffix) {
			value = strings.TrimSpace(strings.TrimSuffix(value, s.suffix))
			mult = s.mult
			break
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", text, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("size must not be negative: %q", text)
	}
	if n > (1<<63-1)/mult {
		return 0, fmt.Errorf("size is too large: %q", text)
	}
	return Size(n * mult), nil
}

// UnmarshalYAML accepts either a string with a suffix or a plain integer.
func (s *Size) UnmarshalYAML(node *yaml.Node) error {
	var text string
	if err := node.Decode(&text); err == nil {
		parsed, err := ParseSize(text)
		if err != nil {
			return err
		}
		*s = parsed
		return nil
	}
	var n int64
	if err := node.Decode(&n); err != nil {
		return fmt.Errorf("size must be a string like 8G or an integer: %w", err)
	}
	if n < 0 {
		return errors.New("size must not be negative")
	}
	*s = Size(n)
	return nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Size) UnmarshalText(text []byte) error {
	parsed, err := ParseSize(string(text))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// MarshalYAML renders the size in GiB when possible.
func (s Size) MarshalYAML() (any, error) {
	const gib = int64(1 << 30)
	if int64(s) != 0 && int64(s)%gib == 0 {
		return fmt.Sprintf("%dG", int64(s)/gib), nil
	}
	return int64(s), nil
}

// planNamePattern keeps plan names usable as GitHub Actions labels.
var planNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

// ValidatePlans checks the plan names and values.
func (c *Config) validatePlans(add func(string, ...any)) {
	for name, plan := range c.Plans {
		if !planNamePattern.MatchString(name) {
			add("plans.%s: name must match %s", name, planNamePattern.String())
		}
		if plan.VCPUCount < 0 {
			add("plans.%s.vcpu_count must not be negative", name)
		}
		if plan.MemSizeMiB < 0 {
			add("plans.%s.mem_size_mib must not be negative", name)
		}
		if plan.DiskSize < 0 {
			add("plans.%s.disk_size must not be negative", name)
		}
	}
}
