package main

import (
	"context"
	"fmt"

	"tenant/internal/config"
	"tenant/internal/orchestra"
)

// The persisted configuration lives in internal/config (TEN-314). These names
// keep package main's call sites as they were; new code may use config
// directly.
type (
	launchConfig        = config.Launch
	logsConfig          = config.Logs
	healthGatingConfig  = config.HealthGating
	turnSchedulerConfig = config.TurnScheduler
	providerConfig      = config.Provider
	routeConfig         = config.Route
	routeProviderConfig = config.RouteProvider
	routeAgentConfig    = config.RouteAgent
	resolvedRouteConfig = config.ResolvedRoute
	routeConfigError    = config.RouteError
	agentProfile        = config.AgentProfile
	authCfg             = config.Auth
	gatewayConfig       = config.Gateway
	dashboardConfig     = config.Dashboard
	peerConfig          = config.Peer
	relayConfig         = config.Relay
	imessageConfig      = config.IMessage
	improveConfig       = config.Improve
	cronJobConfig       = config.CronJob
	skillConfig         = config.Skill
	providerKind        = config.ProviderKind
	credentials         = config.Credentials
	settings            = config.Settings
	execProfileSetting  = config.ExecProfile
)

const (
	defaultEmbedEndpoint = config.DefaultEmbedEndpoint
	defaultEmbedModel    = config.DefaultEmbedModel
	defaultEmbedDim      = config.DefaultEmbedDim
	defaultVLLMToolFmt   = config.DefaultVLLMToolFmt
	currentSchemaVersion = config.CurrentSchemaVersion
	defaultPlanCeiling   = config.DefaultPlanCeiling
)

// providerKinds is the provider catalog; it is never modified, so sharing
// config's map is safe.
var (
	providerKinds = config.ProviderKinds
	providerOrder = config.ProviderOrder
)

func configuredBudgets(lc *launchConfig) orchestra.Budgets { return config.EffectiveBudgets(lc) }

func validAutoAccept(m string) bool { return config.ValidAutoAccept(m) }

func launchConfigPath(cfgDir string) string { return config.Path(cfgDir) }

func loadLaunchConfig(cfgDir string) (*launchConfig, error) { return config.Load(cfgDir) }

// persistLaunchConfig is config.Persist, with every failed save filed,
// whoever made it.
func persistLaunchConfig(cfgDir string, mem *launchConfig, fn func(*launchConfig)) error {
	err := config.Persist(cfgDir, mem, fn)
	if err != nil {
		sysNotSaved.Log(context.Background(), "what", "config.json", "path", config.Path(cfgDir), "err", err)
	}
	return err
}

// errNotSaved wraps the failed save of an operator choice that has already
// taken effect: it is live now, and a restart would silently undo it.
func errNotSaved(what string, err error) error {
	return fmt.Errorf("%s applies now but was not saved, so a restart will undo it: %w", what, err)
}

// persistChoice is persistLaunchConfig for a caller with no error path of its
// own (a persist hook): a failure goes to tell, the operator's status feed,
// because the operator believes the choice was saved. lc nil = no persistence.
func persistChoice(cfgDir string, lc *launchConfig, tell func(string), what string, fn func(*launchConfig)) {
	if lc == nil {
		return
	}
	if err := persistLaunchConfig(cfgDir, lc, fn); err != nil && tell != nil {
		tell(errNotSaved(what, err).Error())
	}
}

func atomicWrite(path string, data []byte) error { return config.AtomicWrite(path, data) }

func resolveRouteConfig(lc *launchConfig) (resolvedRouteConfig, error) {
	return config.ResolveRoute(lc)
}

func routeBlockPresent(lc *launchConfig) bool { return config.RouteBlockPresent(lc) }

func routeEndpointIsLoopback(endpoint string) bool {
	return config.RouteEndpointIsLoopback(endpoint)
}

func credentialsPath(cfgDir string) string { return config.CredentialsPath(cfgDir) }

func loadCredentials(cfgDir string) (*credentials, error) { return config.LoadCredentials(cfgDir) }

func peekCredentials(cfgDir string) (*credentials, error) { return config.PeekCredentials(cfgDir) }

func resolveSecret(cfgDir, providerID string, auth authCfg) string {
	return config.ResolveSecret(cfgDir, providerID, auth)
}

func resolveSecretWith(load func(string) (*credentials, error), cfgDir, providerID string, auth authCfg) string {
	return config.ResolveSecretWith(load, cfgDir, providerID, auth)
}

func settingsPath(cfgDir, agentID string) string { return config.SettingsPath(cfgDir, agentID) }

func loadSettings(cfgDir, agentID string) (*settings, error) {
	return config.LoadSettings(cfgDir, agentID)
}
