package bleephub

import (
	"encoding/json"
	"fmt"
	"os"
)

// loadAppSeedSpecs reads seed specs from BLEEPHUB_SEED_APPS_FILE (a JSON file)
// and BLEEPHUB_SEED_APPS (inline JSON), concatenating both when present.
func loadAppSeedSpecs() ([]AppSeedSpec, error) {
	var specs []AppSeedSpec
	if path := os.Getenv("BLEEPHUB_SEED_APPS_FILE"); path != "" {
		// #nosec G304,G703 -- this is an operator-selected startup config file,
		// never a path supplied by an HTTP client.
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("BLEEPHUB_SEED_APPS_FILE: %w", err)
		}
		var fileSpecs []AppSeedSpec
		if err := json.Unmarshal(b, &fileSpecs); err != nil {
			return nil, fmt.Errorf("BLEEPHUB_SEED_APPS_FILE: invalid JSON: %w", err)
		}
		specs = append(specs, fileSpecs...)
	}
	if inline := os.Getenv("BLEEPHUB_SEED_APPS"); inline != "" {
		var inlineSpecs []AppSeedSpec
		if err := json.Unmarshal([]byte(inline), &inlineSpecs); err != nil {
			return nil, fmt.Errorf("BLEEPHUB_SEED_APPS: invalid JSON: %w", err)
		}
		specs = append(specs, inlineSpecs...)
	}
	return specs, nil
}

// seedConfiguredApps registers the apps described by the seed config. It is
// idempotent across restarts (an app already present — loaded from
// persistence — is left unchanged) and fails loud on a malformed spec, never
// silently degrading.
func (s *Server) seedConfiguredApps() error {
	specs, err := loadAppSeedSpecs()
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if spec.Name == "" {
			return fmt.Errorf("seed app: name is required")
		}
		if spec.ID <= 0 {
			return fmt.Errorf("seed app %q: id must be a positive integer", spec.Name)
		}
		pemKey := spec.PrivateKeyPEM
		if pemKey == "" && spec.PrivateKeyFile != "" {
			// #nosec G304 -- this is an operator-selected startup key file.
			b, err := os.ReadFile(spec.PrivateKeyFile)
			if err != nil {
				return fmt.Errorf("seed app %q: read private_key_pem_file: %w", spec.Name, err)
			}
			pemKey = string(b)
		}
		if pemKey == "" {
			return fmt.Errorf("seed app %q: private_key_pem or private_key_pem_file is required", spec.Name)
		}
		if spec.Owner == "" {
			return fmt.Errorf("seed app %q: owner is required", spec.Name)
		}
		if s.store.LookupUserByLogin(spec.Owner) == nil {
			return fmt.Errorf("seed app %q: owner %q is not an existing user", spec.Name, spec.Owner)
		}

		app, created, err := s.store.SeedApp(spec, pemKey, spec.Owner)
		if err != nil {
			return fmt.Errorf("seed app %q: %w", spec.Name, err)
		}
		if !created {
			s.logger.Info().Int("app_id", app.ID).Str("slug", app.Slug).
				Msg("seed GitHub App already present; left unchanged")
			continue
		}
		s.logger.Info().Int("app_id", app.ID).Str("slug", app.Slug).
			Int("installations", len(spec.Installations)).Msg("seeded pre-registered GitHub App")

		for _, ins := range spec.Installations {
			if ins.Account == "" {
				return fmt.Errorf("seed app %q: installation account is required", spec.Name)
			}
			targetType, targetID, err := s.resolveSeedInstallTarget(ins)
			if err != nil {
				return fmt.Errorf("seed app %q: %w", spec.Name, err)
			}
			inst := s.store.SeedInstallation(app.ID, ins.ID, targetType, targetID, ins.Account, ins.Permissions, ins.Events)
			if inst == nil {
				return fmt.Errorf("seed app %q: failed to create installation on %q", spec.Name, ins.Account)
			}
			s.emitInstallationEvent(app, "created", inst)
			s.logger.Info().Int("app_id", app.ID).Int("installation_id", inst.ID).
				Str("account", ins.Account).Msg("seeded App installation")
		}
	}
	return nil
}

// resolveSeedInstallTarget resolves an installation account login to a real
// target type + id. Seed configuration must name an existing account; startup
// fails instead of inventing an organization or silently installing on id 0.
func (s *Server) resolveSeedInstallTarget(ins InstallationSeedSpec) (string, int, error) {
	if ins.TargetType != "" && ins.TargetType != "Organization" && ins.TargetType != "User" {
		return "", 0, fmt.Errorf("installation account %q: target_type must be Organization or User", ins.Account)
	}
	if org := s.store.GetOrg(ins.Account); org != nil {
		if ins.TargetType == "User" {
			return "", 0, fmt.Errorf("installation account %q is an organization, not a user", ins.Account)
		}
		return "Organization", org.ID, nil
	}
	if u := s.store.LookupUserByLogin(ins.Account); u != nil {
		if ins.TargetType == "Organization" {
			return "", 0, fmt.Errorf("installation account %q is a user, not an organization", ins.Account)
		}
		return "User", u.ID, nil
	}
	want := ins.TargetType
	if want == "" {
		want = "user or organization"
	}
	return "", 0, fmt.Errorf("installation account %q does not resolve to an existing %s", ins.Account, want)
}
