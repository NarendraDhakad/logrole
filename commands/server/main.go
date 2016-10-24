// Command line binary for loading configuration and starting/running the
// logrole server.
package main

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/mail"
	"os"
	"time"

	"github.com/kevinburke/handlers"
	twilio "github.com/kevinburke/twilio-go"
	"github.com/saintpete/logrole/config"
	"github.com/saintpete/logrole/server"
	"github.com/saintpete/logrole/services"
	yaml "gopkg.in/yaml.v2"
)

type fileConfig struct {
	Port           string        `yaml:"port"`
	AccountSid     string        `yaml:"twilio_account_sid"`
	AuthToken      string        `yaml:"twilio_auth_token"`
	Realm          services.Rlm  `yaml:"realm"`
	Timezone       string        `yaml:"timezone"`
	PublicHost     string        `yaml:"public_host"`
	PageSize       uint          `yaml:"page_size"`
	SecretKey      string        `yaml:"secret_key"`
	MaxResourceAge time.Duration `yaml:"max_resource_age"`

	// Need a pointer to a boolean here since we want to be able to distinguish
	// "false" from "omitted"
	ShowMediaByDefault *bool `yaml:"show_media_by_default,omitempty"`

	EmailAddress string `yaml:"email_address"`

	ErrorReporter      string `yaml:"error_reporter,omitempty"`
	ErrorReporterToken string `yaml:"error_reporter_token,omitempty"`

	AuthScheme string `yaml:"auth_scheme"`
	User       string `yaml:"basic_auth_user"`
	Password   string `yaml:"basic_auth_password"`

	GoogleClientID     string `yaml:"google_client_id"`
	GoogleClientSecret string `yaml:"google_client_secret"`
}

var errWrongLength = errors.New("Secret key has wrong length. Should be a 64-byte hex string")

// getSecretKey produces a valid [32]byte secret key or returns an error. If
// hexKey is the empty string, a valid 32 byte key will be randomly generated
// and returned. If hexKey is invalid, an error is returned.
func getSecretKey(hexKey string) (*[32]byte, error) {
	if hexKey == "" {
		return services.NewRandomKey(), nil
	}

	if len(hexKey) != 64 {
		return nil, errWrongLength
	}
	secretKeyBytes, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, err
	}
	secretKey := new([32]byte)
	copy(secretKey[:], secretKeyBytes)
	return secretKey, nil
}

func init() {
	flag.Usage = func() {
		os.Stderr.WriteString(`Logrole: a faster, finer-grained Twilio log viewer

Configuration should be written to a file (default config.yml in the 
current directory) and passed to the binary via the --config flag.

Usage of server:
`)
		flag.PrintDefaults()
		os.Exit(2)
	}
}

func main() {
	cfg := flag.String("config", "config.yml", "Path to a config file")
	flag.Parse()
	if flag.NArg() > 2 {
		os.Stderr.WriteString("too many arguments")
		os.Exit(2)
	}
	if flag.NArg() == 1 {
		switch flag.Arg(0) {
		case "version":
			fmt.Fprintf(os.Stderr, "logrole version %s (twilio-go version %s)\n", server.Version, twilio.Version)
			os.Exit(2)
		case "help":
			flag.Usage()
		case "serve":
			break
		default:
			fmt.Fprintf(os.Stderr, "Unknown flag: %s\n", flag.Arg(0))
			os.Exit(2)
		}
	}
	data, err := ioutil.ReadFile(*cfg)
	c := new(fileConfig)
	if err == nil {
		if err := yaml.Unmarshal(data, c); err != nil {
			handlers.Logger.Error("Couldn't parse config file", "err", err)
			os.Exit(2)
		}
	} else {
		if *cfg != "config.yml" {
			handlers.Logger.Error("Couldn't find config file", "err", err)
			os.Exit(2)
		}
		handlers.Logger.Warn("Couldn't find config file, defaulting to localhost:4114")
		c.Port = config.DefaultPort
		c.Realm = services.Local
	}
	allowHTTP := false
	if c.Realm == services.Local {
		allowHTTP = true
	}
	if c.SecretKey == "" {
		handlers.Logger.Warn("No secret key provided, generating random secret key. Sessions won't persist across restarts")
	}
	secretKey, err := getSecretKey(c.SecretKey)
	if err != nil {
		handlers.Logger.Error(err.Error(), "key", c.SecretKey)
		os.Exit(2)
	}
	if c.MaxResourceAge == 0 {
		c.MaxResourceAge = config.DefaultMaxResourceAge
	}
	var address *mail.Address
	if c.EmailAddress != "" {
		address, err = mail.ParseAddress(c.EmailAddress)
		if err != nil {
			handlers.Logger.Error("Couldn't parse email address", "err", err)
			os.Exit(2)
		}
	}
	if c.ErrorReporter != "" {
		if !services.IsRegistered(c.ErrorReporter) {
			handlers.Logger.Warn("Unknown error reporter, using the noop reporter", "name", c.ErrorReporter)
		}
	}
	reporter := services.GetReporter(c.ErrorReporter, c.ErrorReporterToken)
	var authenticator server.Authenticator
	switch c.AuthScheme {
	case "":
		handlers.Logger.Warn("Disabling basic authentication")
		authenticator = &server.NoopAuthenticator{}
	case "basic":
		if c.User == "" || c.Password == "" {
			handlers.Logger.Error("Cannot run without Basic Auth, set a basic_auth_user")
			os.Exit(2)
		}
		users := make(map[string]string)
		if c.User != "" {
			users[c.User] = c.Password
		}
		authenticator = server.NewBasicAuthAuthenticator("logrole", users)
	case "google":
		var baseURL string
		if allowHTTP {
			baseURL = "http://" + c.PublicHost
		} else {
			baseURL = "https://" + c.PublicHost
		}
		gauthenticator := server.NewGoogleAuthenticator(c.GoogleClientID, c.GoogleClientSecret, baseURL, secretKey)
		gauthenticator.AllowUnencryptedTraffic = allowHTTP
		authenticator = gauthenticator
	default:
		handlers.Logger.Error("Unknown auth scheme", "scheme", c.AuthScheme)
		os.Exit(2)
	}
	client := twilio.NewClient(c.AccountSid, c.AuthToken, nil)
	var location *time.Location
	if c.Timezone == "" {
		handlers.Logger.Info("No timezone provided, defaulting to UTC")
		location = time.UTC
	} else {
		var err error
		location, err = time.LoadLocation(c.Timezone)
		if err != nil {
			handlers.Logger.Error("Couldn't find timezone", "err", err, "timezone", c.Timezone)
			os.Exit(2)
		}
	}
	if c.PageSize == 0 {
		c.PageSize = config.DefaultPageSize
	}
	if c.PageSize > 1000 {
		handlers.Logger.Error("Maximum allowable page size is 1000")
		os.Exit(2)
	}
	if c.ShowMediaByDefault == nil {
		b := true
		c.ShowMediaByDefault = &b
	}

	settings := &server.Settings{
		AllowUnencryptedTraffic: allowHTTP,
		Client:                  client,
		Location:                location,
		PublicHost:              c.PublicHost,
		PageSize:                c.PageSize,
		SecretKey:               secretKey,
		MaxResourceAge:          c.MaxResourceAge,
		ShowMediaByDefault:      *c.ShowMediaByDefault,
		Mailto:                  address,
		Reporter:                reporter,
		Authenticator:           authenticator,
	}
	s := server.NewServer(settings)
	publicMux := http.NewServeMux()
	publicMux.Handle("/", s)
	publicServer := http.Server{
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		Handler:      publicMux,
	}
	listener, err := net.Listen("tcp", fmt.Sprintf(":%s", c.Port))
	if err != nil {
		handlers.Logger.Error("Error listening", "err", err, "port", c.Port)
		os.Exit(2)
	}
	go func(p string) {
		time.Sleep(30 * time.Millisecond)
		handlers.Logger.Info("Started server", "port", p, "public_host", settings.PublicHost)
	}(c.Port)
	publicServer.Serve(listener)
}
