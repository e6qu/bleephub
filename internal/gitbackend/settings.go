package gitbackend

import (
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/e6qu/bleephub/gitstore"
)

// The settings that say where a deployment keeps its bytes. What is stored
// where has one name whichever store it is; how the store is reached is named
// after the driver, and only the chosen driver's settings may be set.
const (
	envObjectStore = "BLEEPHUB_OBJECT_STORE"

	envGitBucket    = "BLEEPHUB_GIT_BUCKET"
	envGitPrefix    = "BLEEPHUB_GIT_PREFIX"
	envObjectBucket = "BLEEPHUB_OBJECT_BUCKET"
	envObjectPrefix = "BLEEPHUB_OBJECT_PREFIX"

	envS3Endpoint         = "BLEEPHUB_S3_ENDPOINT"
	envS3Region           = "BLEEPHUB_S3_REGION"
	envAzureEndpoint      = "BLEEPHUB_AZURE_ENDPOINT"
	envAzureAccount       = "BLEEPHUB_AZURE_ACCOUNT"
	envAzureKey           = "BLEEPHUB_AZURE_KEY"
	envGCSEndpoint        = "BLEEPHUB_GCS_ENDPOINT"
	envGCSCredentialsFile = "BLEEPHUB_GCS_CREDENTIALS_FILE"

	driverS3    = "s3"
	driverAzure = "azure"
	driverGCS   = "gcs"
)

// driverSettings is what each driver reads from the environment. One endpoint
// per driver serves both the git store and the byte store. S3's endpoint is the
// one setting here that may be left unset, and unset is a statement, not a gap
// to be filled: it means AWS S3 itself, in BLEEPHUB_S3_REGION.
type driverSettings struct {
	endpoint string
	required []string
}

var drivers = map[string]driverSettings{
	driverS3:    {endpoint: envS3Endpoint, required: []string{envS3Region}},
	driverAzure: {endpoint: envAzureEndpoint, required: []string{envAzureEndpoint, envAzureAccount, envAzureKey}},
	driverGCS:   {endpoint: envGCSEndpoint, required: []string{envGCSEndpoint, envGCSCredentialsFile}},
}

// all is every setting the driver reads, required or not.
func (d driverSettings) all() []string {
	names := []string{d.endpoint}
	for _, name := range d.required {
		if name != d.endpoint {
			names = append(names, name)
		}
	}
	return names
}

// driverNames lists the drivers as an error message names them.
func driverNames() string {
	return strings.Join(sortedDrivers(), ", ")
}

// Settings is what the environment says about the deployment's object store,
// once it has been found to say one thing.
type Settings struct {
	// Driver is the kind of object store, and empty when the deployment keeps
	// nothing in one.
	Driver string
	// Endpoint is where the chosen driver reaches the store.
	Endpoint string
	// GitBucket and GitPrefix are where git repositories live; ObjectBucket and
	// ObjectPrefix where the service's bytes do. A bucket is a container on
	// Azure. Either bucket may be empty: that store is then not in an object
	// store.
	GitBucket, GitPrefix       string
	ObjectBucket, ObjectPrefix string
	// Options are the storage tunables, which mean the same for every driver.
	Options gitstore.Options
}

// setting reads one variable. Surrounding space is not part of a value: it is
// what a quoted line in an env file leaves behind.
func setting(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// SettingsFromEnv reads the whole storage configuration and refuses one that
// does not say exactly one thing. Nothing is defaulted and nothing is inferred:
// the kind of store is stated, never detected from an endpoint; a bucket comes
// with its prefix, because the two stores may share a bucket and nothing may
// guess where each lives; and a setting of a driver that was not chosen is an
// error rather than an ignored line, since the operator who wrote it believes it
// does something. Every problem is reported at once, the tunables' included, so
// a deployment is corrected in one pass; and the server does not start.
func SettingsFromEnv() (Settings, error) {
	settings := Settings{
		Driver:       setting(envObjectStore),
		GitBucket:    setting(envGitBucket),
		GitPrefix:    setting(envGitPrefix),
		ObjectBucket: setting(envObjectBucket),
		ObjectPrefix: setting(envObjectPrefix),
	}
	var problems []error
	problem := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	stores := []struct{ bucketName, bucket, prefixName, prefix string }{
		{envGitBucket, settings.GitBucket, envGitPrefix, settings.GitPrefix},
		{envObjectBucket, settings.ObjectBucket, envObjectPrefix, settings.ObjectPrefix},
	}
	for _, stored := range stores {
		if stored.bucket != "" && stored.prefix == "" {
			problem("%s is set and %s is not: say where in the bucket the store lives", stored.bucketName, stored.prefixName)
		}
		if stored.bucket == "" && stored.prefix != "" {
			problem("%s is set and %s is not: a prefix is a place in a bucket", stored.prefixName, stored.bucketName)
		}
	}
	if settings.GitBucket != "" && settings.GitBucket == settings.ObjectBucket &&
		settings.GitPrefix != "" && settings.ObjectPrefix != "" && prefixesOverlap(settings.GitPrefix, settings.ObjectPrefix) {
		problem("%s=%q and %s=%q overlap in the bucket %q: each store needs a prefix the other is not inside",
			envGitPrefix, settings.GitPrefix, envObjectPrefix, settings.ObjectPrefix, settings.GitBucket)
	}

	// Repositories live in one place. With both set the bucket used to win and
	// the directory was ignored, which is a deployment running on storage its
	// operator did not mean; which was meant is not for the server to decide.
	if settings.GitBucket != "" && GitDataDir() != "" {
		problem("%s and BLEEPHUB_GIT_DIR are both set: repositories live in a bucket or in a directory, so set one", envGitBucket)
	}

	anyBucket := settings.GitBucket != "" || settings.ObjectBucket != ""
	chosen, known := drivers[settings.Driver]
	switch {
	case settings.Driver == "" && anyBucket:
		problem("%s is not set and a bucket is: say which kind of object store it is in (%s)", envObjectStore, driverNames())
	case settings.Driver != "" && !known:
		problem("%s=%q: want one of %s", envObjectStore, settings.Driver, driverNames())
	case known && !anyBucket:
		problem("%s=%s and neither %s nor %s is set: it names a store that nothing is kept in", envObjectStore, settings.Driver, envGitBucket, envObjectBucket)
	}
	if known {
		settings.Endpoint = setting(chosen.endpoint)
		for _, name := range chosen.required {
			if setting(name) == "" {
				problem("%s is not set, and %s=%s requires it", name, envObjectStore, settings.Driver)
			}
		}
	}
	// With a driver named that does not exist, which driver the others' settings
	// belong beside is not known, and the one problem to report is the name.
	for _, driver := range sortedDrivers() {
		if driver == settings.Driver || (settings.Driver != "" && !known) {
			continue
		}
		for _, name := range drivers[driver].all() {
			if setting(name) != "" {
				problem("%s is set, and it is a setting of the %s driver while %s", name, driver, chosenDriverClause(settings.Driver))
			}
		}
	}

	options, err := OptionsFromEnv()
	if err != nil {
		problems = append(problems, err)
	}
	settings.Options = options
	if len(problems) > 0 {
		return Settings{}, fmt.Errorf("storage configuration: %w", errors.Join(problems...))
	}
	return settings, nil
}

// sortedDrivers lists the drivers in a fixed order, so the problems of one
// configuration are always reported in the same order.
func sortedDrivers() []string {
	names := make([]string, 0, len(drivers))
	for name := range drivers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// chosenDriverClause ends the sentence that refuses another driver's setting.
func chosenDriverClause(driver string) string {
	if driver == "" {
		return envObjectStore + " is not set"
	}
	return fmt.Sprintf("%s=%s", envObjectStore, driver)
}

// prefixesOverlap reports whether one prefix is the other or lies inside it, in
// which case the keys of one store would be among the keys of the other.
func prefixesOverlap(a, b string) bool {
	a = path.Clean("/"+a) + "/"
	b = path.Clean("/"+b) + "/"
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}
