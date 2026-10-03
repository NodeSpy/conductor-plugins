package ghplugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit"
	"github.com/NodeSpy/conductor-plugins/internal/githubkit/ghsource"
)

// ErrAppWebhookMoved is the rejection for a connection still carrying the
// retired app-block webhook keys — the same rejection, in the same words, the
// bundled connector gives, so a config does not mean two things.
var ErrAppWebhookMoved = errors.New("app.webhook_secret moved to webhook.secret " +
	"(and app.verify_signature → webhook.verify_signature) — run 'conductor config migrate'")

// connWire is the connection block as the daemon delivers it: the operator's
// `connectors:` entry, YAML-decoded, secret references resolved.
type connWire struct {
	App struct {
		AppID          flexInt `json:"app_id"`
		PrivateKeyPath string  `json:"private_key_path"`
		// Retired: read only to refuse them (ErrAppWebhookMoved).
		LegacyWebhookSecret string `json:"webhook_secret"`
		LegacyVerifySig     *bool  `json:"verify_signature"`
	} `json:"app"`
	Token   string `json:"token"`
	Webhook struct {
		SmeeURL   string `json:"smee_url"`
		Listen    string `json:"listen"`
		Path      string `json:"path"`
		Secret    string `json:"secret"`
		VerifySig *bool  `json:"verify_signature"`
		// Expose names the exposure connector the engine opened `listen`
		// through; the plugin itself never reads it (the engine does, to
		// decide whether to open an exposure at all). Kept here only so a
		// strict future decoder would not choke on a key the engine sets.
		Expose string `json:"expose"`
		// PublicURL is the exposure's URL, filled in by the engine (see
		// ghsource.WebhookConfig.PublicURL).
		PublicURL string `json:"public_url"`
	} `json:"webhook"`
	Sweep struct {
		Enabled     *bool        `json:"enabled"`
		Interval    flexDuration `json:"interval"`
		MinInterval flexDuration `json:"min_interval"`
		Repos       []string     `json:"repos"`
	} `json:"sweep"`
	Me             ghsource.Actors   `json:"me"`
	Repos          []string          `json:"repos"`
	Identity       ghsource.Identity `json:"identity"`
	ProjectMap     map[string]string `json:"project_map"`
	ProjectRewrite struct {
		Org string `json:"org"`
	} `json:"project_rewrite"`
	APIBase string `json:"api_base"`
}

// Connection is a parsed github connection block: what the source reads
// (ghsource.Connection) and what the verb client reads.
type Connection struct {
	Source ghsource.Connection
	Client githubkit.Config
}

// ParseConnection reads a connection map as the daemon delivers it. It
// accepts the bundled connector's spellings exactly — a duration as "2m" or as
// integer seconds, an id as a number or a numeric string — and refuses the
// retired app-block webhook keys the way the bundled connector does.
func ParseConnection(m map[string]any) (Connection, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return Connection{}, fmt.Errorf("connection: %w", err)
	}
	var w connWire
	if err := json.Unmarshal(b, &w); err != nil {
		return Connection{}, fmt.Errorf("connection: %w", err)
	}
	if w.App.LegacyWebhookSecret != "" || w.App.LegacyVerifySig != nil {
		return Connection{}, ErrAppWebhookMoved
	}
	src := ghsource.Connection{
		App:   ghsource.AppConfig{AppID: int64(w.App.AppID), PrivateKeyPath: w.App.PrivateKeyPath},
		Token: w.Token,
		Webhook: ghsource.WebhookConfig{
			SmeeURL: w.Webhook.SmeeURL, Listen: w.Webhook.Listen, Path: w.Webhook.Path,
			Secret: w.Webhook.Secret, VerifySig: w.Webhook.VerifySig,
			PublicURL: w.Webhook.PublicURL,
		},
		Sweep: ghsource.SweepConfig{
			Enabled: w.Sweep.Enabled, Interval: time.Duration(w.Sweep.Interval),
			MinInterval: time.Duration(w.Sweep.MinInterval), Repos: w.Sweep.Repos,
		},
		Me:             w.Me,
		Repos:          w.Repos,
		Identity:       w.Identity,
		ProjectMap:     w.ProjectMap,
		ProjectRewrite: ghsource.ProjectRewrite{Org: w.ProjectRewrite.Org},
		APIBase:        w.APIBase,
	}
	cl := githubkit.Config{Token: w.Token, WriteToken: w.Identity.WriteToken, ReadToken: w.Identity.ReadToken, APIBase: w.APIBase}
	if w.App.AppID > 0 && w.App.PrivateKeyPath != "" {
		cl.App = &githubkit.AppConfig{AppID: int64(w.App.AppID), PrivateKeyPath: w.App.PrivateKeyPath}
	}
	return Connection{Source: src, Client: cl}, nil
}

// flexInt decodes a JSON number or a numeric string.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		i, err := strconv.ParseInt(n.String(), 10, 64)
		*f = flexInt(i)
		return err
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("want an integer, got %s", b)
	}
	if s == "" {
		*f = 0
		return nil
	}
	i, err := strconv.ParseInt(s, 10, 64)
	*f = flexInt(i)
	return err
}

// flexDuration decodes "2m"-style strings (with an optional leading day
// component, "1d12h") or integer seconds — the shapes conductor's
// config.Duration accepts.
type flexDuration time.Duration

func (d *flexDuration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if s == "" {
			*d = 0
			return nil
		}
		v, err := parseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = flexDuration(v)
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("want a duration, got %s", b)
	}
	*d = flexDuration(time.Duration(n) * time.Second)
	return nil
}

// parseDuration is time.ParseDuration plus a leading integer day component.
func parseDuration(s string) (time.Duration, error) {
	if i := strings.IndexByte(s, 'd'); i > 0 {
		if days, err := strconv.Atoi(s[:i]); err == nil {
			rest := time.Duration(0)
			if tail := s[i+1:]; tail != "" {
				r, err := time.ParseDuration(tail)
				if err != nil {
					return 0, err
				}
				rest = r
			}
			return time.Duration(days)*24*time.Hour + rest, nil
		}
	}
	return time.ParseDuration(s)
}
