package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// httpClient verifies TLS normally, but if a server omits its intermediate
// certificates (floyd.wcbn.org does), it fetches them from the leaf's
// Authority Information Access URLs, as browsers do, and verifies again.
var httpClient = newHTTPClient()

func newHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{
		// Verification still happens, in VerifyConnection.
		InsecureSkipVerify: true,
		VerifyConnection:   verifyWithAIA,
	}
	return &http.Client{Transport: t}
}

var (
	aiaCache  sync.Map // URL -> *x509.Certificate
	aiaClient = &http.Client{Timeout: 15 * time.Second}
)

func verifyWithAIA(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("tls: server sent no certificates")
	}
	leaf := cs.PeerCertificates[0]
	opts := x509.VerifyOptions{DNSName: cs.ServerName, Intermediates: x509.NewCertPool()}
	for _, c := range cs.PeerCertificates[1:] {
		opts.Intermediates.AddCert(c)
	}
	_, err := leaf.Verify(opts)
	var unknown x509.UnknownAuthorityError
	if err == nil || !errors.As(err, &unknown) {
		return err
	}
	// Walk up the chain fetching missing issuers, a few levels at most.
	cert := leaf
	for range 3 {
		issuer, ferr := fetchIssuer(cert)
		if ferr != nil {
			return fmt.Errorf("%w (fetching intermediate: %v)", err, ferr)
		}
		opts.Intermediates.AddCert(issuer)
		if _, err = leaf.Verify(opts); err == nil || !errors.As(err, &unknown) {
			return err
		}
		cert = issuer
	}
	return err
}

func fetchIssuer(cert *x509.Certificate) (*x509.Certificate, error) {
	if len(cert.IssuingCertificateURL) == 0 {
		return nil, errors.New("certificate has no issuer URL")
	}
	var lastErr error
	for _, u := range cert.IssuingCertificateURL {
		if c, ok := aiaCache.Load(u); ok {
			return c.(*x509.Certificate), nil
		}
		c, err := downloadCert(u)
		if err != nil {
			lastErr = err
			continue
		}
		aiaCache.Store(u, c)
		return c, nil
	}
	return nil, lastErr
}

func downloadCert(u string) (*x509.Certificate, error) {
	resp, err := aiaClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if block, _ := pem.Decode(data); block != nil {
		data = block.Bytes
	}
	return x509.ParseCertificate(data)
}
