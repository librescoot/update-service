package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
)

const DefaultV2Feed = "https://downloads.librescoot.org/updates/v2/releases.json"
const CanaryV2Feed = "https://downloads.librescoot.org/updates/v2/canary.json"
const OTAProtocol = 2

type FeedPolicy struct {
	Schema  int    `json:"schema_version"`
	URL     string `json:"url"`
	Channel string `json:"channel"`
}

func FeedPolicyPath(component string) string {
	return filepath.Join("/data/ota", component, "feed.json")
}
func (p FeedPolicy) Validate() error {
	u, err := url.Parse(p.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || filepath.Ext(u.Path) != ".json" || p.Schema != 2 || (p.Channel != "stable" && p.Channel != "testing") {
		return fmt.Errorf("invalid exclusive v2 feed policy")
	}
	return nil
}
func LoadFeedPolicy(path string) (FeedPolicy, error) {
	var p FeedPolicy
	f, err := os.Open(path)
	if err != nil {
		return p, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return p, err
	}
	if st.Size() > 4096 {
		return p, fmt.Errorf("feed policy too large")
	}
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&p); err != nil {
		return p, err
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return p, fmt.Errorf("trailing data in feed policy")
	}
	return p, p.Validate()
}
func SaveFeedPolicy(path string, p FeedPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.CreateTemp(dir, ".feed-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// FeedURL and FeedPolicyFile are immutable after startup. The selected channel
// is re-read from the durable policy so transient Redis settings cannot unbind it.
func (c *Config) BindFeed(path string, p FeedPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	c.FeedURL = p.URL
	c.FeedPolicyFile = path
	c.Channel = p.Channel
	c.FallbackChannel = p.Channel
	return nil
}
func (c *Config) FeedChannel() (string, error) {
	p, err := LoadFeedPolicy(c.FeedPolicyFile)
	if err != nil {
		return "", err
	}
	if p.URL != c.FeedURL {
		return "", fmt.Errorf("feed URL changed; restart updater to apply")
	}
	return p.Channel, nil
}
func (c *Config) SetFeedChannel(channel string) error {
	p, err := LoadFeedPolicy(c.FeedPolicyFile)
	if err != nil {
		return err
	}
	if p.URL != c.FeedURL {
		return fmt.Errorf("feed URL changed; restart updater to apply")
	}
	p.Channel = channel
	if err := SaveFeedPolicy(c.FeedPolicyFile, p); err != nil {
		return err
	}
	c.channelMu.Lock()
	c.Channel = channel
	c.channelMu.Unlock()
	return nil
}
