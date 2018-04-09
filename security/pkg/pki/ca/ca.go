// Copyright 2017 Istio Authors
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

package ca

import (
	"encoding/pem"
	"fmt"
	"time"

	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/probe"
	"istio.io/istio/security/pkg/pki/util"
)

const (
	// istioCASecretType is the Istio secret annotation type.
	istioCASecretType = "istio.io/ca-root"

	// cACertChainID is the CA certificate chain file.
	cACertID = "ca-cert.pem"
	// cAPrivateKeyID is the private key file of CA.
	cAPrivateKeyID = "ca-key.pem"
	// cASecret stores the key/cert of self-signed CA for persistency purpose.
	cASecret = "istio-ca-secret"

	// The size of a private key for a self-signed Istio CA.
	caKeySize = 2048
)

// cATypes is the enum for the CA type.
type cATypes int

const (
	// SelfSignedCA means the Istio CA uses a self signed certificate.
	selfSignedCA cATypes = iota
	// PluggedCertCA means the Istio CA uses a operator-specified key/cert.
	pluggedCertCA
)

// CertificateAuthority contains methods to be supported by a CA.
type CertificateAuthority interface {
	// Sign generates a certificate for a workload or CA, from the given CSR and TTL.
	Sign(csrPEM []byte, ttl time.Duration) ([]byte, error)
	// SignCAServerCert generates a certificate for the CA server (to serve the CSR).
	// TODO(myidpt): Remove this and add forCA in Sign().
	SignCAServerCert(csrPEM []byte, ttl time.Duration) ([]byte, error)
	// GetCAKeyCertBundle returns the KeyCertBundle used by CA.
	GetCAKeyCertBundle() util.KeyCertBundle
}

// IstioCAOptions holds the configurations for creating an Istio CA.
// TODO(myidpt): remove IstioCAOptions.
type IstioCAOptions struct {
	CAType cATypes

	CertTTL    time.Duration
	MaxCertTTL time.Duration

	multicluster bool

	KeyCertBundle util.KeyCertBundle

	LivenessProbeOptions *probe.Options
	ProbeCheckInterval   time.Duration
}

// IstioCA generates keys and certificates for Istio identities.
type IstioCA struct {
	certTTL    time.Duration
	maxCertTTL time.Duration

	multicluster bool

	keyCertBundle util.KeyCertBundle

	livenessProbe *probe.Probe
}

// NewSelfSignedIstioCAOptions returns a new IstioCAOptions instance using self-signed certificate.
func NewSelfSignedIstioCAOptions(caCertTTL, certTTL, maxCertTTL time.Duration, multicluster bool, org string,
	namespace string, core corev1.SecretsGetter) (caOpts *IstioCAOptions, err error) {
	// For the first time the CA is up, it generates a self-signed key/cert pair and write it to
	// cASecret. For subsequent restart, CA will reads key/cert from cASecret.
	caSecret, scrtErr := core.Secrets(namespace).Get(cASecret, metav1.GetOptions{})
	caOpts = &IstioCAOptions{
		CAType:       selfSignedCA,
		CertTTL:      certTTL,
		MaxCertTTL:   maxCertTTL,
		multicluster: multicluster,
	}
	if scrtErr != nil {
		log.Infof("Failed to get secret (error: %s), will create one", scrtErr)

		options := util.CertOptions{
			TTL:          caCertTTL,
			Org:          org,
			IsCA:         true,
			IsSelfSigned: true,
			RSAKeySize:   caKeySize,
		}
		pemCert, pemKey, ckErr := util.GenCertKeyFromOptions(options)
		if ckErr != nil {
			return nil, fmt.Errorf("unable to generate CA cert and key for self-signed CA (%v)", ckErr)
		}

		if caOpts.KeyCertBundle, err = util.NewVerifiedKeyCertBundleFromPem(pemCert, pemKey, nil, pemCert); err != nil {
			return nil, fmt.Errorf("failed to create CA KeyCertBundle (%v)", err)
		}

		// Rewrite the key/cert back to secret so they will be persistent when CA restarts.
		secret := &apiv1.Secret{
			Data: map[string][]byte{
				cACertID:       pemCert,
				cAPrivateKeyID: pemKey,
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      cASecret,
				Namespace: namespace,
			},
			Type: istioCASecretType,
		}
		if _, err = core.Secrets(namespace).Create(secret); err != nil {
			log.Errorf("Failed to write secret to CA (error: %s). This CA will not persist when restart.", err)
		}
	} else {
		if caOpts.KeyCertBundle, err = util.NewVerifiedKeyCertBundleFromPem(caSecret.Data[cACertID],
			caSecret.Data[cAPrivateKeyID], nil, caSecret.Data[cACertID]); err != nil {
			return nil, fmt.Errorf("failed to create CA KeyCertBundle (%v)", err)
		}
	}

	return caOpts, nil
}

// NewPluggedCertIstioCAOptions returns a new IstioCAOptions instance using given certificate.
func NewPluggedCertIstioCAOptions(certChainFile, signingCertFile, signingKeyFile, rootCertFile string,
	certTTL, maxCertTTL time.Duration, multicluster bool) (caOpts *IstioCAOptions, err error) {
	caOpts = &IstioCAOptions{
		CAType:       pluggedCertCA,
		CertTTL:      certTTL,
		MaxCertTTL:   maxCertTTL,
		multicluster: multicluster,
	}
	if caOpts.KeyCertBundle, err = util.NewVerifiedKeyCertBundleFromFile(
		signingCertFile, signingKeyFile, certChainFile, rootCertFile); err != nil {
		return nil, fmt.Errorf("failed to create CA KeyCertBundle (%v)", err)
	}
	return caOpts, nil
}

// NewIstioCA returns a new IstioCA instance.
func NewIstioCA(opts *IstioCAOptions) (*IstioCA, error) {
	ca := &IstioCA{
		certTTL:       opts.CertTTL,
		maxCertTTL:    opts.MaxCertTTL,
		keyCertBundle: opts.KeyCertBundle,
		multicluster:  opts.multicluster,
		livenessProbe: probe.NewProbe(),
	}

	return ca, nil
}

// Sign takes a PEM-encoded CSR and returns a signed certificate. If the CA is a multicluster CA,
// the signed certificate is a CA certificate (CA:TRUE in X509v3 Basic Constraints), otherwise, it is a workload
// certificate.
func (ca *IstioCA) Sign(csrPEM []byte, ttl time.Duration) ([]byte, error) {
	return ca.sign(csrPEM, ttl, ca.multicluster)
}

// SignCAServerCert signs the certificate for the Istio CA server (to serve the CSR, etc).
func (ca *IstioCA) SignCAServerCert(csrPEM []byte, ttl time.Duration) ([]byte, error) {
	return ca.sign(csrPEM, ttl, false)
}

// GetCAKeyCertBundle returns the KeyCertBundle for the CA.
func (ca *IstioCA) GetCAKeyCertBundle() util.KeyCertBundle {
	return ca.keyCertBundle
}

func (ca *IstioCA) sign(csrPEM []byte, ttl time.Duration, forCA bool) ([]byte, error) {
	signingCert, signingKey, _, _ := ca.keyCertBundle.GetAll()
	if signingCert == nil {
		return nil, fmt.Errorf("Istio CA is not ready") // nolint
	}

	csr, err := util.ParsePemEncodedCSR(csrPEM)
	if err != nil {
		return nil, err
	}

	// If the requested TTL is greater than maxCertTTL, return an error
	if ttl.Seconds() > ca.maxCertTTL.Seconds() {
		return nil, fmt.Errorf(
			"requested TTL %s is greater than the max allowed TTL %s", ttl, ca.maxCertTTL)
	}

	certBytes, err := util.GenCertFromCSR(csr, signingCert, csr.PublicKey, *signingKey, ttl, forCA)
	if err != nil {
		return nil, err
	}

	block := &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certBytes,
	}
	cert := pem.EncodeToMemory(block)

	return cert, nil
}
