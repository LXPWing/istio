// Copyright Istio Authors
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
package bootstrap

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/keycertbundle"
	"istio.io/istio/pilot/pkg/server"
	"istio.io/istio/pilot/pkg/serviceregistry"
	kubecontroller "istio.io/istio/pilot/pkg/serviceregistry/kube/controller"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/testcerts"
	"istio.io/pkg/filewatcher"
)

func TestNewServerCertInit(t *testing.T) {
	configDir, err := ioutil.TempDir("", "test_istiod_config")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.RemoveAll(configDir)
	}()

	certsDir, err := ioutil.TempDir("", "test_istiod_certs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.RemoveAll(certsDir)
	}()

	certFile := filepath.Join(certsDir, "cert-file.pem")
	keyFile := filepath.Join(certsDir, "key-file.pem")
	caCertFile := filepath.Join(certsDir, "ca-cert.pem")

	// load key and cert files.
	if err := ioutil.WriteFile(certFile, testcerts.ServerCert, 0o644); err != nil { // nolint: vetshadow
		t.Fatalf("WriteFile(%v) failed: %v", certFile, err)
	}
	if err := ioutil.WriteFile(keyFile, testcerts.ServerKey, 0o644); err != nil { // nolint: vetshadow
		t.Fatalf("WriteFile(%v) failed: %v", keyFile, err)
	}
	if err := ioutil.WriteFile(caCertFile, testcerts.CACert, 0o644); err != nil { // nolint: vetshadow
		t.Fatalf("WriteFile(%v) failed: %v", caCertFile, err)
	}

	cases := []struct {
		name         string
		tlsOptions   *TLSOptions
		enableCA     bool
		certProvider string
		expNewCert   bool
		expCert      []byte
		expKey       []byte
	}{
		{
			name: "Load from existing DNS cert",
			tlsOptions: &TLSOptions{
				CertFile:   certFile,
				KeyFile:    keyFile,
				CaCertFile: caCertFile,
			},
			enableCA:     false,
			certProvider: KubernetesCAProvider,
			expNewCert:   false,
			expCert:      testcerts.ServerCert,
			expKey:       testcerts.ServerKey,
		},
		{
			name: "Create new DNS cert using Istiod",
			tlsOptions: &TLSOptions{
				CertFile:   "",
				KeyFile:    "",
				CaCertFile: "",
			},
			enableCA:     true,
			certProvider: IstiodCAProvider,
			expNewCert:   true,
			expCert:      []byte{},
			expKey:       []byte{},
		},
		{
			name:         "No DNS cert created because CA is disabled",
			tlsOptions:   &TLSOptions{},
			enableCA:     false,
			certProvider: IstiodCAProvider,
			expNewCert:   false,
			expCert:      []byte{},
			expKey:       []byte{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			os.Setenv("PILOT_CERT_PROVIDER", c.certProvider)
			features.EnableCAServer = c.enableCA
			args := NewPilotArgs(func(p *PilotArgs) {
				p.Namespace = "istio-system"
				p.ServerOptions = DiscoveryServerOptions{
					// Dynamically assign all ports.
					HTTPAddr:       ":0",
					MonitoringAddr: ":0",
					GRPCAddr:       ":0",
					SecureGRPCAddr: ":0",
					TLSOptions:     *c.tlsOptions,
				}
				p.RegistryOptions = RegistryOptions{
					FileDir: configDir,
				}

				// Include all of the default plugins
				p.Plugins = DefaultPlugins
				p.ShutdownDuration = 1 * time.Millisecond
			})
			g := NewWithT(t)
			s, err := NewServer(args)
			g.Expect(err).To(Succeed())
			stop := make(chan struct{})
			g.Expect(s.Start(stop)).To(Succeed())
			defer func() {
				close(stop)
				s.WaitUntilCompletion()
				features.EnableCAServer = true
				os.Setenv("PILOT_CERT_PROVIDER", IstiodCAProvider)
			}()

			if c.expNewCert {
				if istiodCert, err := s.getIstiodCertificate(nil); istiodCert == nil || err != nil {
					t.Errorf("Istiod failed to generate new DNS cert")
				}
			} else {
				if len(c.expCert) != 0 {
					if !checkCert(t, s, c.expCert, c.expKey) {
						t.Errorf("Istiod certifiate does not match the expectation")
					}
				} else {
					if cert, _ := s.getIstiodCertificate(nil); cert != nil {
						t.Errorf("Istiod should not generate new DNS cert")
					}
				}
			}
		})
	}
}

func TestReloadIstiodCert(t *testing.T) {
	dir, err := ioutil.TempDir("", "istiod_certs")
	stop := make(chan struct{})
	s := &Server{
		fileWatcher:             filewatcher.NewWatcher(),
		server:                  server.New(),
		istiodCertBundleWatcher: keycertbundle.NewWatcher(),
	}

	defer func() {
		close(stop)
		_ = s.fileWatcher.Close()
		_ = os.RemoveAll(dir)
	}()
	if err != nil {
		t.Fatalf("TempDir() failed: %v", err)
	}

	certFile := filepath.Join(dir, "cert-file.yaml")
	keyFile := filepath.Join(dir, "key-file.yaml")
	caFile := filepath.Join(dir, "ca-file.yaml")

	// load key and cert files.
	if err := ioutil.WriteFile(certFile, testcerts.ServerCert, 0o644); err != nil { // nolint: vetshadow
		t.Fatalf("WriteFile(%v) failed: %v", certFile, err)
	}
	if err := ioutil.WriteFile(keyFile, testcerts.ServerKey, 0o644); err != nil { // nolint: vetshadow
		t.Fatalf("WriteFile(%v) failed: %v", keyFile, err)
	}

	if err := ioutil.WriteFile(caFile, testcerts.CACert, 0644); err != nil { // nolint: vetshadow
		t.Fatalf("WriteFile(%v) failed: %v", caFile, err)
	}

	tlsOptions := TLSOptions{
		CertFile:   certFile,
		KeyFile:    keyFile,
		CaCertFile: caFile,
	}

	// setup cert watches.
	if err = s.initCertificateWatches(tlsOptions); err != nil {
		t.Fatalf("initCertificateWatches failed: %v", err)
	}

	if err = s.server.Start(stop); err != nil {
		t.Fatalf("Could not invoke startFuncs: %v", err)
	}

	// Validate that the certs are loaded.
	if !checkCert(t, s, testcerts.ServerCert, testcerts.ServerKey) {
		t.Errorf("Istiod certifiate does not match the expectation")
	}

	// Update cert/key files.
	if err := ioutil.WriteFile(tlsOptions.CertFile, testcerts.RotatedCert, 0o644); err != nil { // nolint: vetshadow
		t.Fatalf("WriteFile(%v) failed: %v", tlsOptions.CertFile, err)
	}
	if err := ioutil.WriteFile(tlsOptions.KeyFile, testcerts.RotatedKey, 0o644); err != nil { // nolint: vetshadow
		t.Fatalf("WriteFile(%v) failed: %v", tlsOptions.KeyFile, err)
	}

	g := NewWithT(t)

	// Validate that istiod cert is updated.
	g.Eventually(func() bool {
		return checkCert(t, s, testcerts.RotatedCert, testcerts.RotatedKey)
	}, "10s", "100ms").Should(BeTrue())
}

func TestNewServer(t *testing.T) {
	// All of the settings to apply and verify. Currently just testing domain suffix,
	// but we should expand this list.
	cases := []struct {
		name             string
		domain           string
		expectedDomain   string
		enableSecureGRPC bool
		jwtRule          string
	}{
		{
			name:           "default domain",
			domain:         "",
			expectedDomain: constants.DefaultKubernetesDomain,
		},
		{
			name:           "default domain with JwtRule",
			domain:         "",
			expectedDomain: constants.DefaultKubernetesDomain,
			jwtRule:        `{"issuer": "foo", "jwks_uri": "baz", "audiences": ["aud1", "aud2"]}`,
		},
		{
			name:           "override domain",
			domain:         "mydomain.com",
			expectedDomain: "mydomain.com",
		},
		{
			name:             "override default secured grpc port",
			domain:           "",
			expectedDomain:   constants.DefaultKubernetesDomain,
			enableSecureGRPC: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			configDir, err := ioutil.TempDir("", "TestNewServer")
			if err != nil {
				t.Fatal(err)
			}

			defer func() {
				_ = os.RemoveAll(configDir)
			}()

			var secureGRPCPort int
			if c.enableSecureGRPC {
				secureGRPCPort, err = findFreePort()
				if err != nil {
					t.Errorf("unable to find a free port: %v", err)
					return
				}
			}

			args := NewPilotArgs(func(p *PilotArgs) {
				p.Namespace = "istio-system"
				p.ServerOptions = DiscoveryServerOptions{
					// Dynamically assign all ports.
					HTTPAddr:       ":0",
					MonitoringAddr: ":0",
					GRPCAddr:       ":0",
					SecureGRPCAddr: fmt.Sprintf(":%d", secureGRPCPort),
				}
				p.RegistryOptions = RegistryOptions{
					KubeOptions: kubecontroller.Options{
						DomainSuffix: c.domain,
					},
					FileDir: configDir,
				}

				// Include all of the default plugins
				p.Plugins = DefaultPlugins
				p.ShutdownDuration = 1 * time.Millisecond

				p.JwtRule = c.jwtRule
			})

			g := NewWithT(t)
			s, err := NewServer(args)
			g.Expect(err).To(Succeed())
			stop := make(chan struct{})
			g.Expect(s.Start(stop)).To(Succeed())
			defer func() {
				close(stop)
				s.WaitUntilCompletion()
			}()

			g.Expect(s.environment.DomainSuffix).To(Equal(c.expectedDomain))

			if c.enableSecureGRPC {
				tcpAddr := s.secureGrpcAddress
				_, port, err := net.SplitHostPort(tcpAddr)
				if err != nil {
					t.Errorf("invalid SecureGrpcListener addr %v", err)
				}
				g.Expect(port).To(Equal(strconv.Itoa(secureGRPCPort)))
			}
		})
	}
}

func TestIstiodCipherSuites(t *testing.T) {
	cases := []struct {
		name               string
		serverCipherSuites []uint16
		clientCipherSuites []uint16
		expectSuccess      bool
	}{
		{
			name:          "default cipher suites",
			expectSuccess: true,
		},
		{
			name:               "client and istiod cipher suites match",
			serverCipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
			clientCipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
			expectSuccess:      true,
		},
		{
			name:               "client and istiod cipher suites mismatch",
			serverCipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
			clientCipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384, tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384},
			expectSuccess:      false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			configDir, err := ioutil.TempDir("", "TestIstiodCipherSuites")
			if err != nil {
				t.Fatal(err)
			}

			defer func() {
				_ = os.RemoveAll(configDir)
			}()

			port, err := findFreePort()
			if err != nil {
				t.Errorf("unable to find a free port: %v", err)
				return
			}

			args := NewPilotArgs(func(p *PilotArgs) {
				p.Namespace = "istio-system"
				p.ServerOptions = DiscoveryServerOptions{
					// Dynamically assign all ports.
					HTTPAddr:       ":0",
					MonitoringAddr: ":0",
					GRPCAddr:       ":0",
					HTTPSAddr:      fmt.Sprintf(":%d", port),
					TLSOptions: TLSOptions{
						CipherSuits: c.serverCipherSuites,
					},
				}
				p.RegistryOptions = RegistryOptions{
					KubeConfig: "config",
					FileDir:    configDir,
				}

				// Include all of the default plugins
				p.Plugins = DefaultPlugins
				p.ShutdownDuration = 1 * time.Millisecond
			})

			g := NewWithT(t)
			s, err := NewServer(args, func(s *Server) {
				s.kubeClient = kube.NewFakeClient()
			})
			g.Expect(err).To(Succeed())

			stop := make(chan struct{})
			g.Expect(s.Start(stop)).To(Succeed())
			defer func() {
				close(stop)
				s.WaitUntilCompletion()
			}()

			// wait for the https server start
			time.Sleep(time.Second)

			httpsReadyClient := &http.Client{
				Timeout: time.Second,
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{
						InsecureSkipVerify: true,
						CipherSuites:       c.clientCipherSuites,
						MinVersion:         tls.VersionTLS12,
						MaxVersion:         tls.VersionTLS12,
					},
				},
			}

			req := &http.Request{
				Method: http.MethodGet,
				URL: &url.URL{
					Scheme: "https",
					Host:   s.httpsServer.Addr,
					Path:   HTTPSHandlerReadyPath,
				},
			}
			response, err := httpsReadyClient.Do(req)
			if c.expectSuccess && err != nil {
				t.Errorf("expect success but got err %v", err)
				return
			}
			if !c.expectSuccess && err == nil {
				t.Errorf("expect failure but succeeded")
				return
			}
			if response != nil {
				response.Body.Close()
			}
		})
	}
}

func TestNewServerWithMockRegistry(t *testing.T) {
	cases := []struct {
		name             string
		registry         string
		expectedRegistry serviceregistry.ProviderID
	}{
		{
			name:             "Mock Registry",
			registry:         "Mock",
			expectedRegistry: serviceregistry.Mock,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			configDir, err := ioutil.TempDir("", "TestNewServer")
			if err != nil {
				t.Fatal(err)
			}

			defer func() {
				_ = os.RemoveAll(configDir)
			}()

			args := NewPilotArgs(func(p *PilotArgs) {
				p.Namespace = "istio-system"

				// As the same with args in main go of pilot-discovery
				p.InjectionOptions = InjectionOptions{
					InjectionDirectory: "./var/lib/istio/inject",
				}

				p.ServerOptions = DiscoveryServerOptions{
					// Dynamically assign all ports.
					HTTPAddr:       ":0",
					MonitoringAddr: ":0",
					GRPCAddr:       ":0",
				}

				p.RegistryOptions = RegistryOptions{
					Registries: []string{c.registry},
					FileDir:    configDir,
				}

				// Include all of the default plugins
				p.Plugins = DefaultPlugins
				p.ShutdownDuration = 1 * time.Millisecond
			})

			g := NewWithT(t)
			s, err := NewServer(args)
			g.Expect(err).To(Succeed())

			stop := make(chan struct{})
			g.Expect(s.Start(stop)).To(Succeed())
			defer func() {
				close(stop)
				s.WaitUntilCompletion()
			}()

			g.Expect(s.ServiceController().GetRegistries()[1].Provider()).To(Equal(c.expectedRegistry))
		})
	}
}

func TestInitOIDC(t *testing.T) {
	tests := []struct {
		name      string
		expectErr bool
		jwtRule   string
	}{
		{
			name:      "valid jwt rule",
			expectErr: false,
			jwtRule:   `{"issuer": "foo", "jwks_uri": "baz", "audiences": ["aud1", "aud2"]}`,
		},
		{
			name:      "invalid jwt rule",
			expectErr: true,
			jwtRule:   "invalid",
		},
		{
			name:      "jwt rule with invalid audiences",
			expectErr: true,
			// audiences must be a string array
			jwtRule: `{"issuer": "foo", "jwks_uri": "baz", "audiences": "aud1"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := &PilotArgs{JwtRule: tt.jwtRule}

			_, err := initOIDC(args, "domain-foo")
			gotErr := err != nil
			if gotErr != tt.expectErr {
				t.Errorf("expect error is %v while actual error is %v", tt.expectErr, gotErr)
			}
		})
	}
}

func checkCert(t *testing.T, s *Server, cert, key []byte) bool {
	t.Helper()
	actual, _ := s.getIstiodCertificate(nil)
	expected, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatalf("fail to load test certs.")
	}
	return bytes.Equal(actual.Certificate[0], expected.Certificate[0])
}

func findFreePort() (int, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("invalid listen address: %q", ln.Addr().String())
	}
	return tcpAddr.Port, nil
}
