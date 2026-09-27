package main

import (
	"tenant/internal/config"
	"tenant/internal/orchestra"
)

// The persisted configuration lives in internal/config (TEN-314). These names
// keep package main's call sites as they were; new code may use config
// directly.
type (
	launchConfig        = config.Launch
	logsConfig          = config.Logs
	privacyConfig       = config.Privacy
	runTraceConfig      = config.RunTrace
	healthGatingConfig  = config.HealthGating
	goalConfig          = config.Goal
	turnSchedulerConfig = config.TurnScheduler
	providerConfig      = config.Provider
	routeConfig         = config.Route
	routeProviderConfig = config.RouteProvider
	routeRuleConfig     = config.RouteRule
	routeRuleWhen       = config.RouteRuleWhen
	routeAgentConfig    = config.RouteAgent
	resolvedRouteConfig = config.ResolvedRoute
	routeConfigError    = config.RouteError
	agentProfile        = config.AgentProfile
	authCfg             = config.Auth
	gatewayConfig       = config.Gateway
	dashboardConfig     = config.Dashboard
	tailscaleConfig     = config.Tailscale
	peerConfig          = config.Peer
	relayConfig         = config.Relay
	imessageConfig      = config.IMessage
	improveConfig       = config.Improve
	cronConfig          = config.Cron
	cronJobConfig       = config.CronJob
	skillConfig         = config.Skill
	providerKind        = config.ProviderKind
	credentials         = config.Credentials
	settings            = config.Settings
	execProfileSetting  = config.ExecProfile
)

const (
	defaultEmbedEndpoint            = config.DefaultEmbedEndpoint
	defaultEmbedModel               = config.DefaultEmbedModel
	defaultEmbedDim                 = config.DefaultEmbedDim
	defaultRouteMode                = config.DefaultRouteMode
	defaultRouteTimeoutMS           = config.DefaultRouteTimeoutMS
	defaultRouteModel               = config.DefaultRouteModel
	defaultRouteMinComplexityForBig = config.DefaultRouteMinComplexityForBig
	defaultVLLMToolFmt              = config.DefaultVLLMToolFmt
	currentSchemaVersion            = config.CurrentSchemaVersion
	defaultPlanCeiling              = config.DefaultPlanCeiling
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

func persistLaunchConfig(cfgDir string, mem *launchConfig, fn func(*launchConfig)) error {
	return config.Persist(cfgDir, mem, fn)
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
