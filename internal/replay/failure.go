package replay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"syscall"
	"time"
)

// FailureKind classifies why an exchange did not produce a usable response.
type FailureKind string

const (
	FailTimeout        FailureKind = "timeout"
	FailDNS            FailureKind = "dns"
	FailTLS            FailureKind = "tls"
	FailRefused        FailureKind = "connection_refused"
	FailReset          FailureKind = "connection_reset"
	FailNetwork        FailureKind = "network"
	FailBodyLimit      FailureKind = "body_limit"
	FailCanceled       FailureKind = "canceled"
	FailInvalidRequest FailureKind = "invalid_request"
)

// Failure describes a transport or limit failure. Messages never include
// request headers or bodies.
type Failure struct {
	Kind    FailureKind `json:"kind"`
	Message string      `json:"message"`
}

func (f *Failure) Error() string { return string(f.Kind) + ": " + f.Message }

// classify maps a client error to a FailureKind. parent is the caller's
// context: if it was canceled, the run is being stopped and the failure is
// reported as canceled rather than blamed on the target.
func classify(parent context.Context, err error, timeout time.Duration) *Failure {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err // drop the "Get <url>:" prefix
	}
	msg := err.Error()

	if parent.Err() != nil {
		return &Failure{Kind: FailCanceled, Message: "run canceled before the exchange completed"}
	}

	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return &Failure{Kind: FailTimeout, Message: fmt.Sprintf("no complete response within %s", timeout)}
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return &Failure{Kind: FailDNS, Message: msg}
	}

	var (
		certErr      *tls.CertificateVerificationError
		unknownAuth  x509.UnknownAuthorityError
		hostnameErr  x509.HostnameError
		invalidCert  x509.CertificateInvalidError
		recordHeader tls.RecordHeaderError
		alert        tls.AlertError
	)
	if errors.As(err, &certErr) || errors.As(err, &unknownAuth) || errors.As(err, &hostnameErr) ||
		errors.As(err, &invalidCert) || errors.As(err, &recordHeader) || errors.As(err, &alert) {
		return &Failure{Kind: FailTLS, Message: msg}
	}

	var errno syscall.Errno
	if errors.As(err, &errno) {
		for _, e := range refusedErrnos {
			if errno == e {
				return &Failure{Kind: FailRefused, Message: msg}
			}
		}
		for _, e := range resetErrnos {
			if errno == e {
				return &Failure{Kind: FailReset, Message: msg}
			}
		}
	}

	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &Failure{Kind: FailNetwork, Message: "connection closed before a complete response was received"}
	}
	return &Failure{Kind: FailNetwork, Message: msg}
}
