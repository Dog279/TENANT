// Command releasekey makes, checks and uses the project key the main
// releases sign checksums.txt with (TEN-542). Run from the repository root:
//
//	go run ./scripts/releasekey keygen -out FILE   # the owner, once per key
//	go run ./scripts/releasekey check              # release.yml, before GoReleaser
//	go run ./scripts/releasekey sign FILE          # release.yml: writes FILE.sig
//
// check and sign read the key from the environment, never from an argument:
// TENANT_RELEASE_SIGNING_KEY (the repository secret: standard base64 of the
// 32-byte Ed25519 seed) and TENANT_RELEASE_PUBKEYS (the repository variable
// stamped into the binaries: selfupdate.ParseKeys's form). Neither is ever
// printed.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"tenant/internal/fsperm"
	"tenant/internal/selfupdate"
)

const (
	envSecret = "TENANT_RELEASE_SIGNING_KEY"
	envPublic = "TENANT_RELEASE_PUBKEYS"
)

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "releasekey:", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: releasekey keygen -out FILE | check | sign FILE")
	}
	switch args[0] {
	case "keygen":
		fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
		path := fs.String("out", "", "the file the private key is written to; it must not exist")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *path == "" || fs.NArg() > 0 {
			return errors.New("usage: releasekey keygen -out FILE")
		}
		return keygen(*path, out)
	case "check":
		if len(args) != 1 {
			return errors.New("usage: releasekey check")
		}
		priv, _, err := loadKey(getenv)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "sign=%t\n", priv != nil)
		return nil
	case "sign":
		if len(args) != 2 {
			return errors.New("usage: releasekey sign FILE")
		}
		return sign(args[1], getenv)
	}
	return fmt.Errorf("unknown mode %q: keygen, check or sign", args[0])
}

// keygen writes a new key's seed to path, owner-only, refusing a file that
// exists, and prints the public key for the repository variable.
func keygen(path string, out io.Writer) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	f, err := fsperm.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return fmt.Errorf("the private key is written only to a new file: %w", err)
	}
	_, err = io.WriteString(f, base64.StdEncoding.EncodeToString(priv.Seed())+"\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	fmt.Fprintf(out, "The private key is in %s (readable by you only).\n"+
		"Put the file's contents in the repository secret %s, then keep the file offline or delete it.\n"+
		"The public key, for the repository variable %s (when replacing a key, add it after the old one with a comma):\n%s\n",
		path, envSecret, envPublic, base64.StdEncoding.EncodeToString(pub))
	return nil
}

// loadKey is the signing key and the public keys from the environment. With
// neither set it is nil, nil: the release is not signed with a project key.
// One without the other fails: a variable without the secret would make
// binaries that refuse every update, and a secret without the variable
// signatures nothing checks. The secret's public half must be one of the
// keys the binaries carry.
func loadKey(getenv func(string) string) (ed25519.PrivateKey, []ed25519.PublicKey, error) {
	secret, public := strings.TrimSpace(getenv(envSecret)), getenv(envPublic)
	switch {
	case secret == "" && public == "":
		return nil, nil, nil
	case secret == "":
		return nil, nil, fmt.Errorf("the variable %s is set but the secret %s is not: this release's binaries would refuse every update", envPublic, envSecret)
	case public == "":
		return nil, nil, fmt.Errorf("the secret %s is set but the variable %s is not: the binaries would carry no key to check the signature with", envSecret, envPublic)
	}
	keys, err := selfupdate.ParseKeys(public)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", envPublic, err)
	}
	seed, err := base64.StdEncoding.Strict().DecodeString(secret)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, nil, fmt.Errorf("%s is not standard base64 of a %d-byte Ed25519 seed", envSecret, ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	if !slices.ContainsFunc(keys, func(k ed25519.PublicKey) bool { return k.Equal(priv.Public()) }) {
		return nil, nil, fmt.Errorf("the public half of %s is not among the keys in %s", envSecret, envPublic)
	}
	return priv, keys, nil
}

// sign writes path.sig, the project signature over path's exact bytes, and
// checks it the way a binary will before it is published.
func sign(path string, getenv func(string) string) error {
	priv, keys, err := loadKey(getenv)
	if err != nil {
		return err
	}
	if priv == nil {
		return fmt.Errorf("%s and %s are not set: nothing to sign with", envSecret, envPublic)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(f, selfupdate.MaxChecksums+1))
	f.Close()
	if err != nil {
		return err
	}
	sig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data)) + "\n")
	if _, err := selfupdate.VerifyProject(data, sig, keys); err != nil {
		return err
	}
	return os.WriteFile(path+".sig", sig, 0o644)
}
