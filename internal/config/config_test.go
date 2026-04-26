package config

import "testing"

func TestInferChannelFromVersion(t *testing.T) {
	cases := []struct {
		name    string
		version string
		want    string
	}{
		{"testing tag", "testing-20260313T142530", "testing"},
		{"testing tag lowercase t", "testing-20260426t013148", "testing"},
		{"testing with codename suffix", "testing-20260313T142530 (codename)", "testing"},
		{"nightly tag", "nightly-20260313T142530", "nightly"},
		{"nightly tag lowercase t", "nightly-20260426t013148", "nightly"},
		{"stable v-prefixed", "v1.2.3", "stable"},
		{"stable digit-prefixed", "1.2.3", "stable"},
		{"stable v-prefixed with codename", "v1.2.3 (codename)", "stable"},
		{"custom-nightly is not nightly", "custom-nightly-20260313T142530-some-branch", ""},
		{"empty string", "", ""},
		{"bare codename", "(none)", ""},
		{"unknown prefix", "preview-20260313T142530", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InferChannelFromVersion(tc.version)
			if got != tc.want {
				t.Errorf("InferChannelFromVersion(%q) = %q, want %q", tc.version, got, tc.want)
			}
		})
	}
}

func TestIsValidChannel(t *testing.T) {
	valid := []string{"stable", "testing", "nightly"}
	for _, ch := range valid {
		if !IsValidChannel(ch) {
			t.Errorf("IsValidChannel(%q) = false, want true", ch)
		}
	}

	invalid := []string{"", "STABLE", "Nightly", "custom-nightly", "preview", "foo"}
	for _, ch := range invalid {
		if IsValidChannel(ch) {
			t.Errorf("IsValidChannel(%q) = true, want false", ch)
		}
	}
}
