// Copyright 2024 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file.
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

package certificates

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"sync"
	"time"

	v1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	tlsCertValidity = time.Hour * time.Duration(24)   // 24 hours
	tlsCertRotation = time.Hour * time.Duration(1)    // Rotate 1 hour before expire
	caCertValidity  = time.Hour * time.Duration(168)  // 1w
	caCertRotation  = time.Hour * time.Duration(10)   // Rotate 10 hours before expire
	ticker          = time.Minute * time.Duration(10) // Check for certificate expiration every 10 minutes

	deploymentsWebhookSuffix  = "-deployments.gardener.cloud"
	statefulSetsWebhookSuffix = "-statefulsets.gardener.cloud"
	podsWebhookSuffix         = "-pods.gardener.cloud"
)

type certManager struct {
	// Path to store the certificates
	certPath string
	// SAN records for the TLS bundle
	dnsNames []string
	// Webhook resource reference
	webhookKey types.NamespacedName
	// K8S Client for updating the webhook CABundle resource
	client client.Client
	// Managed Certificates
	ca, tls *bundle
	manager.LeaderElectionRunnable
	ctx context.Context
}

// certManager is a controller-runtime runnable, managing a tuple of CA and TLS certificates for the webhook
var _ manager.Runnable = &certManager{}
var _ manager.LeaderElectionRunnable = &certManager{}

var webhookUpdateRetry = wait.Backoff{
	Steps:    5,
	Duration: 1 * time.Second,
	Factor:   1.0,
	Jitter:   0.5,
}

var _log = logf.Log.WithName("certificate-manager")

// New creates a new controller-runtime runnable providing
// bundle rotation for the service endpoint of the mutating webhook
func New(certPath string, objectKey types.NamespacedName, c client.Client, config *rest.Config) (manager.Runnable, error) {

	runnable := &certManager{
		certPath:   certPath,
		client:     c,
		webhookKey: objectKey,
	}

	var cancel context.CancelFunc
	runnable.ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dnsNames := []string{objectKey.Name}
	if objectKey.Namespace != "" {
		prefix := objectKey.Name + "." + objectKey.Namespace
		dnsNames = append(dnsNames,
			prefix,
			prefix+".svc",
			prefix+".svc.cluster",
			prefix+".svc.cluster.local",
		)
	}
	runnable.dnsNames = dnsNames

	var err error
	// Check if there is valid CA bundle
	if runnable.ca, _ = loadCAFromDisk(certPath); runnable.ca == nil {
		if runnable.ca, err = generateCACert(certPath, realCertOps{}); err != nil {
			return nil, err
		}
		if err = runnable.setupWebhooksCABundles(config, objectKey); err != nil {
			return nil, err
		}
	}

	// Check if there is valid TLS bundle
	if runnable.tls, _ = loadTLSFromDisk(certPath); runnable.tls == nil {
		if runnable.tls, err = generateTLSCert(certPath, realCertOps{}, dnsNames, runnable.ca); err != nil {
			return nil, err
		}
	}

	return runnable, nil
}

func (c *certManager) NeedLeaderElection() bool {
	return false
}

func (c *certManager) Start(ctx context.Context) error {
	_log.Info("Starting webhook certificate manager")
	c.ctx = ctx
	runnableWaitGroup := &sync.WaitGroup{} // Wait group for the certificate manager
	runnableWaitGroup.Add(1)
	go func() {
		defer runnableWaitGroup.Done()
		wg := &sync.WaitGroup{}
		wg.Add(2)
		go c.rotateCACert(ctx, wg)  // Start the CA bundle rotation
		go c.rotateTLSCert(ctx, wg) // Start the TLS bundle rotation
		<-ctx.Done()                // Waiting for the controller-runtime.Manager to close the context
		_log.Info("Shutting down the webhook certificate manager")
		wg.Wait()
	}()

	runnableWaitGroup.Wait() // Done with the certificate manager
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c.cleanUpMutatingWebhookConfiguration(ctx) // Clean up the webhook CABundles
	return nil
}

// setupWebhooksCABundles is invoked during runnable initialization and before the controller manager Start method is called.
// The purpose is to generate an initial set of CA nad TLS certificates bundle and to update the webhook CABundle resource
// Since the client.Client is not present at this moment, we need to construct a new client.
func (c *certManager) setupWebhooksCABundles(config *rest.Config, objectKey types.NamespacedName) error {
	cl, err := client.New(config, client.Options{})
	if err != nil {
		return err
	}

	mutatingWebhook := &v1.MutatingWebhookConfiguration{}
	return retry.RetryOnConflict(webhookUpdateRetry, func() error {
		if err = cl.Get(c.ctx, objectKey, mutatingWebhook); err != nil {
			return err
		}
		for i, w := range mutatingWebhook.Webhooks {
			if w.Name != c.webhookKey.Name+deploymentsWebhookSuffix &&
				w.Name != c.webhookKey.Name+statefulSetsWebhookSuffix &&
				w.Name != c.webhookKey.Name+podsWebhookSuffix {
				continue
			}

			b, err := c.updateCABundles(w.Name, w.ClientConfig.CABundle)
			if err != nil {
				_log.Error(err, "Error updating webhook CA bundle")
				break
			}
			mutatingWebhook.Webhooks[i].ClientConfig.CABundle = b
		}
		return cl.Update(context.Background(), mutatingWebhook)
	})
}

// updateWebhookConfiguration is invoked when runnable (certManager) is running.
// At this moment the client.Client is present, and we can use it to update the webhook CABundle resource without
// creating a new client.
func (c *certManager) updateWebhookConfiguration(ctx context.Context) error {

	webhook := &v1.MutatingWebhookConfiguration{}
	return retry.RetryOnConflict(webhookUpdateRetry, func() error {
		if err := c.client.Get(c.ctx, c.webhookKey, webhook); err != nil {
			return err
		}
		for i, w := range webhook.Webhooks {
			if w.Name != c.webhookKey.Name+deploymentsWebhookSuffix &&
				w.Name != c.webhookKey.Name+statefulSetsWebhookSuffix &&
				w.Name != c.webhookKey.Name+podsWebhookSuffix {
				continue
			}

			b, err := c.updateCABundles(w.Name, w.ClientConfig.CABundle)
			if err != nil {
				_log.Error(err, "Error updating webhook CA bundle")
				break
			}
			webhook.Webhooks[i].ClientConfig.CABundle = b
		}
		return c.client.Update(ctx, webhook)
	})

}

// rotateTLSCert is a loops over ticker and checks if the TLS bundle is expired.
func (c *certManager) rotateTLSCert(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	tlsTicker := time.NewTicker(ticker)
	defer tlsTicker.Stop()
	ops := realCertOps{}
OuterLoop:
	for {
		select {
		case <-tlsTicker.C:
			expire := time.Now().Add(tlsCertRotation).UTC()
			if expire.Before(c.tls.cert.NotAfter) {
				_log.Info("TLS bundle is valid",
					"serial", c.tls.cert.SerialNumber.String(),
					"expire", expire,
					"certificate notAfter", c.tls.cert.NotAfter)
				continue
			}
			t, err := generateTLSCert(c.certPath, ops, c.dnsNames, c.ca)
			if err != nil {
				_log.Error(err, "Error rotating TLS bundle")
			}
			c.tls.key = t.key
			c.tls.cert = t.cert
			_log.Info("TLS bundle is rotated",
				"serial", c.tls.cert.SerialNumber.String(),
				"certificate notAfter", c.tls.cert.NotAfter)
		case <-ctx.Done():
			_log.Info("Shutting down the TLS bundle rotation")
			break OuterLoop
		}
	}
}

// rotateCACert is a loops over ticker and checks if the CA bundle is expired.
func (c *certManager) rotateCACert(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	caTicker := time.NewTicker(ticker)
	defer caTicker.Stop()
	ops := realCertOps{}
OuterLoop:
	for {
		select {
		case <-caTicker.C:
			expire := time.Now().Add(caCertRotation).UTC()
			if expire.Before(c.ca.cert.NotAfter) {
				_log.Info("CA bundle is valid",
					"serial", c.ca.cert.SerialNumber.String(),
					"expire", expire,
					"certificate notAfter", c.ca.cert.NotAfter)
				continue
			}
			crt, err := generateCACert(c.certPath, ops)
			if err != nil {
				_log.Error(err, "Error rotating CA bundle")
			}
			c.ca.key = crt.key
			c.ca.cert = crt.cert
			_log.Info("CA bundle is rotated",
				"serial", c.ca.cert.SerialNumber.String(),
				"certificate notAfter", c.ca.cert.NotAfter)

			if err := c.updateWebhookConfiguration(ctx); err != nil {
				_log.Error(err, "Error updating webhook CA bundle")
			}

			t, _ := generateTLSCert(c.certPath, ops, c.dnsNames, c.ca)
			c.tls.key = t.key
			c.tls.cert = t.cert
			_log.Info("TLS bundle is rotated",
				"serial", c.tls.cert.SerialNumber.String(),
				"certificate notAfter", c.tls.cert.NotAfter)
		case <-ctx.Done():
			_log.Info("Shutting down the CA bundle rotation")
			break OuterLoop
		}
	}

}

func (c *certManager) updateCABundles(name string, caBundle []byte) ([]byte, error) {
	_log.V(9).Info("Updating webhook CA bundle", "webhook", name)
	updatedCAs, currentCAs := []x509.Certificate{}, []x509.Certificate{}
	for len(caBundle) > 0 {
		var block *pem.Block
		block, caBundle = pem.Decode(caBundle)
		// no pem block is found
		if block == nil {
			_log.Info("No bundle is present in the CA Bundle",
				"webhook", name,
			)
			break
		}

		if block.Type != "CERTIFICATE" {
			continue
		}

		crt, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		// Add bundle to the temp storage
		currentCAs = append(currentCAs, *crt)
	}

	// Clean up expired certs or the cert with the currently generated CN
	for _, ca := range currentCAs {
		// ca is before now, hence it is expired
		if ca.NotAfter.Compare(time.Now().UTC()) == -1 {
			_log.Info("Certificate is expired, skipping from temp storage",
				"webhook", name,
				"serial", ca.SerialNumber.String(),
			)
			continue
		}
		updatedCAs = append(updatedCAs, ca)
	}
	// add the new CA bundle
	updatedCAs = append(updatedCAs, *c.ca.cert)

	var caBundleSlice []byte
	for _, ca := range updatedCAs {
		block := &pem.Block{
			Type:  "CERTIFICATE",
			Bytes: ca.Raw,
		}

		caBundleSlice = append(caBundleSlice, pem.EncodeToMemory(block)...)

		_log.V(9).Info("Certificate added to the CA Bundle",
			"webhook", name,
			"commonName", ca.Subject.CommonName,
			"serial", ca.SerialNumber.String(),
			"size", len(caBundleSlice),
		)
	}
	_log.Info("Certificate CA Bundle length", "length", len(updatedCAs))
	return caBundleSlice, nil
}

func (c *certManager) cleanUpMutatingWebhookConfiguration(ctx context.Context) {
	webhook := &v1.MutatingWebhookConfiguration{}

	if err := retry.RetryOnConflict(webhookUpdateRetry, func() error {
		if err := c.client.Get(ctx, c.webhookKey, webhook); err != nil {
			return err
		}
		c.cleanWebhookCABundles(webhook)
		return c.client.Update(ctx, webhook)
	}); err != nil {
		// panic if we cannot get/update the webhook
		_log.Error(err, "Error updating webhook")
		os.Exit(1)
	}

}

func (c *certManager) cleanWebhookCABundles(oidcWebhook *v1.MutatingWebhookConfiguration) {

	for i, w := range oidcWebhook.Webhooks {
		if w.Name != c.webhookKey.Name+deploymentsWebhookSuffix &&
			w.Name != c.webhookKey.Name+statefulSetsWebhookSuffix &&
			w.Name != c.webhookKey.Name+podsWebhookSuffix {
			continue
		}

		b, err := c.removeCABundle(w.Name, w.ClientConfig.CABundle)
		if err != nil {
			_log.Error(err, "Error updating webhook CA Bundle")
			break
		}
		oidcWebhook.Webhooks[i].ClientConfig.CABundle = b
	}
}

func (c *certManager) removeCABundle(name string, caBundle []byte) ([]byte, error) {
	currentCAs := []x509.Certificate{}
	for len(caBundle) > 0 {
		var block *pem.Block
		block, caBundle = pem.Decode(caBundle)
		// no pem block is found
		if block == nil {
			_log.Info("No bundle is present in the CA Bundle",
				"webhook", name,
			)
			break
		}

		if block.Type != "CERTIFICATE" {
			continue
		}

		crt, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		// Add bundle to the temp storage
		if c.ca.cert.SerialNumber.String() != crt.SerialNumber.String() {
			currentCAs = append(currentCAs, *crt)
		}
	}

	var caBundleSlice []byte
	for _, ca := range currentCAs {
		block := &pem.Block{
			Type:  "CERTIFICATE",
			Bytes: ca.Raw,
		}

		caBundleSlice = append(caBundleSlice, pem.EncodeToMemory(block)...)

	}
	_log.Info("Certificate CA Bundle length",
		"webhook", name,
		"length", len(currentCAs),
	)
	return caBundleSlice, nil
}
