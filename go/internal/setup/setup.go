// Package setup turns a Mako home folder into a working config and trackers, saying what keeps it from working.
package setup

import (
	"errors"
	"fmt"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// Build reads home's mako.config.json and .env. It always returns a config (the defaults when the file is
// missing or wrong) and the trackers that could be made; problems say what to fix in the settings.
func Build(home string) (*config.Config, []trackers.Tracker, []string) {
	cfg, err := config.Load(home)
	i18n.SetLang(cfg.Language)
	var problems []string
	switch {
	case errors.Is(err, config.ErrMissing):
		problems = append(problems, i18n.T("problem.notSetUp"))
	case err != nil:
		problems = append(problems, "mako.config.json: "+err.Error())
	}
	var ts []trackers.Tracker
	for _, tc := range cfg.Trackers {
		t, err := trackers.New(tc, cfg)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", tc.ID, err))
			continue
		}
		ts = append(ts, t)
	}
	if err == nil && len(cfg.Trackers) == 0 {
		problems = append(problems, i18n.T("problem.noTrackers"))
	}
	return cfg, ts, problems
}
