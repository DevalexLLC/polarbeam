package ca

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
)

// parseAllPEMCerts parses every CERTIFICATE block in data.
func parseAllPEMCerts(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		return nil, errors.New("no CERTIFICATE blocks found")
	}
	return certs, nil
}
