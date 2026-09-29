// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package injector

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"
	"time"
)

// TestMintedCertificateVerifies tests that a minted server certificate is a
// usable key pair and chains to the minted CA for every name the apiserver
// may call the Service by.
func TestMintedCertificateVerifies(t *testing.T) {
	now := time.Now()
	_, caPEM, keyPEM, certPEM, err := mintCertificates(now)
	if err != nil {
		t.Fatalf("mintCertificates: %v", err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("server key pair unusable: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatalf("CA certificate does not parse")
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("server certificate does not parse: %v", err)
	}
	for _, name := range []string{fullName, Name + "." + serviceNamespace, Name} {
		if _, err := cert.Verify(x509.VerifyOptions{
			DNSName:     name,
			Roots:       roots,
			CurrentTime: now,
			KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			t.Errorf("server certificate does not verify for %q: %v", name, err)
		}
	}
	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "evil.example", Roots: roots, CurrentTime: now}); err == nil {
		t.Errorf("server certificate verifies for an unrelated name")
	}
}

// TestMintedCertificatesAreFresh tests that two mints share no CA, so that a
// key from one run cannot be used against the next.
func TestMintedCertificatesAreFresh(t *testing.T) {
	_, ca1, _, _, err := mintCertificates(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, ca2, _, _, err := mintCertificates(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if string(ca1) == string(ca2) {
		t.Errorf("two mints produced the same CA")
	}
}

// TestLoadOrMint tests the three states of the working directory: no
// certificate files (mint), all of them (load), and some (refuse).
func TestLoadOrMint(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	_, ca, _, _, err := loadOrMintCertificates()
	if err != nil || len(ca) == 0 {
		t.Fatalf("with no files: got %d-byte CA, err %v; want a minted CA", len(ca), err)
	}

	for i, name := range certFiles {
		if err := os.WriteFile(name, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		_, gotCA, _, _, err := loadOrMintCertificates()
		if i < len(certFiles)-1 {
			if err == nil {
				t.Errorf("with %d of %d files: no error, want a refusal", i+1, len(certFiles))
			}
			continue
		}
		if err != nil || string(gotCA) != "caCert.pem" {
			t.Errorf("with all files: got CA %q, err %v; want the file's contents", gotCA, err)
		}
	}
}
