// Command release signs a MonoPanel release. The panel accepts a package only
// when the checksum list it came with carries a signature from the key the
// server was told to trust, so a release is signed exactly once, here.
//
//	go run ./scripts/release keygen
//	MONOPANEL_RELEASE_KEY=… go run ./scripts/release sign dist/SHA256SUMS
//	go run ./scripts/release verify --key <public> dist/SHA256SUMS dist/SHA256SUMS.sig
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"

	"monopanel/internal/updater"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen()
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: release keygen | sign [--key-file f] <file> | verify --key <public> <file> <signature>")
	os.Exit(2)
}

// keygen prints a fresh pair: the private key goes into the repository secret
// the release workflow reads, the public one into config.yaml on every server.
func keygen() error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	fmt.Println("# private key — GitHub secret MONOPANEL_RELEASE_KEY (never commit it)")
	fmt.Println(base64.StdEncoding.EncodeToString(priv))
	fmt.Println()
	fmt.Println("# public key — mp update trust --key <…> on every server")
	fmt.Println(base64.StdEncoding.EncodeToString(pub))
	return nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyFile := fs.String("key-file", "", "file holding the base64 private key (default: $MONOPANEL_RELEASE_KEY)")
	out := fs.String("out", "", "signature file (default: <file>.sig)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("sign takes one file")
	}
	raw := os.Getenv("MONOPANEL_RELEASE_KEY")
	if *keyFile != "" {
		b, err := os.ReadFile(*keyFile)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	key, err := parsePrivate(raw)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(key, data))
	path := *out
	if path == "" {
		path = fs.Arg(0) + ".sig"
	}
	if err := os.WriteFile(path, []byte(sig+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "signed", fs.Arg(0), "->", path)
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	key := fs.String("key", "", "base64 public key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("verify takes a file and a signature")
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(fs.Arg(1))
	if err != nil {
		return err
	}
	if err := updater.VerifySums(*key, data, sig); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "signature ok")
	return nil
}

func parsePrivate(s string) (ed25519.PrivateKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("no private key: set MONOPANEL_RELEASE_KEY or pass --key-file")
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("private key is not base64: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("private key is %d bytes, want %d", len(raw), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(raw), nil
}
