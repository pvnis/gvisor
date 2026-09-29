// Copyright 2020 The gVisor Authors.
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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"time"
)

var (
	caKey      []byte
	caCert     []byte
	serverKey  []byte
	serverCert []byte
)

// certFiles are read from the working directory, if present, in the order
// caKey, caCert, serverKey, serverCert.
var certFiles = []string{"caKey.pem", "caCert.pem", "serverKey.pem", "serverCert.pem"}

func init() {
	var err error
	caKey, caCert, serverKey, serverCert, err = loadOrMintCertificates()
	if err != nil {
		panic(fmt.Errorf("unable to create certificates: %v", err))
	}
}

// loadOrMintCertificates returns the certificates in certFiles if all of them
// are present, and otherwise mints a fresh CA and a server certificate it
// signs.
//
// Minting is the normal case. The CA is discarded when the process exits, and
// CreateConfiguration writes each new one into the MutatingWebhookConfiguration
// it registers or adopts, so nothing has to be generated or distributed ahead
// of time. The files remain for an operator who wants a CA of their own.
func loadOrMintCertificates() (caKey, caCert, serverKey, serverCert []byte, err error) {
	contents := make([][]byte, len(certFiles))
	missing := 0
	for i, name := range certFiles {
		contents[i], err = os.ReadFile(name)
		if errors.Is(err, fs.ErrNotExist) {
			missing++
			continue
		}
		if err != nil {
			return nil, nil, nil, nil, err
		}
	}
	switch missing {
	case 0:
		return contents[0], contents[1], contents[2], contents[3], nil
	case len(certFiles):
		return mintCertificates(time.Now())
	default:
		// Minting would silently replace the operator's CA with ours.
		return nil, nil, nil, nil, fmt.Errorf("only some of %v are present; supply all of them or none", certFiles)
	}
}

// certLifetime is how long a minted certificate is valid. It outlives any
// process that could hold it, since each start mints its own.
const certLifetime = 5 * 365 * 24 * time.Hour

// mintCertificates creates a CA and a server certificate for the webhook's
// Service, signed by it, and returns both keys and certificates PEM-encoded.
func mintCertificates(now time.Time) (caKeyPEM, caCertPEM, serverKeyPEM, serverCertPEM []byte, err error) {
	caPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: Name + "-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caPriv.PublicKey, caPriv)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	serverPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	serverTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: fullName},
		// The apiserver calls the Service by its cluster DNS name; the
		// shorter forms are what an in-cluster client may use.
		DNSNames:    []string{fullName, Name + "." + serviceNamespace, Name},
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    now.Add(certLifetime),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTmpl, ca, &serverPriv.PublicKey, caPriv)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	caKeyDER, err := x509.MarshalECPrivateKey(caPriv)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	serverKeyDER, err := x509.MarshalECPrivateKey(serverPriv)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: caKeyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		nil
}
