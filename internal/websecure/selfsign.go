package websecure

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/net/idna"
)

const selfSignerCAMagicName = "__ca__"

// maxSelfSignedHosts caps the certificates minted on demand, one per TLS
// server name (D15). The client picks the name before any authentication, so
// without a cap anyone could fill the store, and the disk, with names.
const maxSelfSignedHosts = 32

type SelfSigner struct {
	store *CertStore
	log   *zerolog.Logger

	caInfo pkix.Name

	DefaultDomain string
	DefaultOrg    string
	DefaultOU     string

	// LocalNames, if set, returns the device's own names (host name, FQDN):
	// they are minted even past maxSelfSignedHosts (D15).
	LocalNames func() []string

	capWarned bool // guarded by store.certLock
}

func NewSelfSigner(
	store *CertStore,
	log *zerolog.Logger,
	defaultDomain,
	defaultOrg,
	defaultOU,
	caName string,
) *SelfSigner {
	return &SelfSigner{
		store:         store,
		log:           log,
		DefaultDomain: defaultDomain,
		DefaultOrg:    defaultOrg,
		DefaultOU:     defaultOU,
		caInfo: pkix.Name{
			CommonName:         caName,
			Organization:       []string{defaultOrg},
			OrganizationalUnit: []string{defaultOU},
		},
	}
}

// createSelfSignedCert returns hostname's certificate, minting and storing it
// (and the CA) if needed. TLS handshakes call it concurrently: the store's
// lock is held throughout, so the map is never read while it is written and
// only one CA is ever minted (D15). A capped name (one the client chose) is
// minted only below maxSelfSignedHosts, unless it is one of the device's own.
func (s *SelfSigner) createSelfSignedCert(hostname string, capped bool) *tls.Certificate {
	s.store.certLock.Lock()
	defer s.store.certLock.Unlock()
	return s.createSelfSignedCertLocked(hostname, capped)
}

func (s *SelfSigner) createSelfSignedCertLocked(hostname string, capped bool) *tls.Certificate {
	if tlsCert := s.store.certificates[hostname]; tlsCert != nil {
		return tlsCert
	}

	// check if hostname is the CA magic name
	var ca *tls.Certificate
	if hostname != selfSignerCAMagicName {
		if capped && hostname != s.DefaultDomain && s.mintedHostsLocked() >= maxSelfSignedHosts && !s.isLocalName(hostname) {
			if !s.capWarned {
				s.capWarned = true
				s.log.Warn().Int("max", maxSelfSignedHosts).Str("hostname", hostname).
					Msg("Too many self-signed certificates, using the default domain's for new names (logged once)")
			}
			return s.createSelfSignedCertLocked(s.DefaultDomain, false)
		}
		ca = s.createSelfSignedCertLocked(selfSignerCAMagicName, false)
		if ca == nil {
			s.log.Error().Msg("Failed to get CA certificate")
			return nil
		}
	}

	s.log.Info().Str("hostname", hostname).Msg("Creating self-signed certificate")

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		s.log.Error().Err(err).Msg("Failed to generate private key")
		return nil
	}

	notBefore := time.Now()
	notAfter := notBefore.AddDate(1, 0, 0)

	serialNumber, err := generateSerialNumber()
	if err != nil {
		s.log.Error().Err(err).Msg("Failed to generate serial number")
		return nil
	}

	dnsName := hostname
	ip := net.ParseIP(hostname)
	if ip != nil {
		dnsName = s.DefaultDomain
	}

	// set up CSR
	isCA := hostname == selfSignerCAMagicName
	subject := pkix.Name{
		CommonName:         hostname,
		Organization:       []string{s.DefaultOrg},
		OrganizationalUnit: []string{s.DefaultOU},
	}
	keyUsage := x509.KeyUsageDigitalSignature
	extKeyUsage := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}

	// check if hostname is the CA magic name, and if so, set the subject to the CA info
	if isCA {
		subject = s.caInfo
		keyUsage |= x509.KeyUsageCertSign
		extKeyUsage = append(extKeyUsage, x509.ExtKeyUsageClientAuth)
		notAfter = notBefore.AddDate(10, 0, 0)
	}

	cert := x509.Certificate{
		SerialNumber:          serialNumber,
		Subject:               subject,
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  isCA,
		KeyUsage:              keyUsage,
		ExtKeyUsage:           extKeyUsage,
		BasicConstraintsValid: true,
	}

	// set up DNS names and IP addresses
	if !isCA {
		cert.DNSNames = []string{dnsName}
		if ip != nil {
			cert.IPAddresses = []net.IP{ip}
		}
	}

	// set up parent certificate
	parent := &cert
	parentPriv := priv
	if ca != nil {
		parent, err = x509.ParseCertificate(ca.Certificate[0])
		if err != nil {
			s.log.Error().Err(err).Msg("Failed to parse parent certificate")
			return nil
		}
		parentPriv = ca.PrivateKey.(*ecdsa.PrivateKey)
	}

	certBytes, err := x509.CreateCertificate(rand.Reader, &cert, parent, &priv.PublicKey, parentPriv)
	if err != nil {
		s.log.Error().Err(err).Msg("Failed to create certificate")
		return nil
	}

	tlsCert := &tls.Certificate{
		Certificate: [][]byte{certBytes},
		PrivateKey:  priv,
	}
	if ca != nil {
		tlsCert.Certificate = append(tlsCert.Certificate, ca.Certificate...)
	}

	s.store.certificates[hostname] = tlsCert
	s.store.saveCertificateLocked(hostname)

	return tlsCert
}

// isLocalName reports whether hostname is one of LocalNames.
func (s *SelfSigner) isLocalName(hostname string) bool {
	if s.LocalNames == nil {
		return false
	}
	for _, name := range s.LocalNames() {
		if name != "" && strings.EqualFold(name, hostname) {
			return true
		}
	}
	return false
}

// mintedHostsLocked counts the stored certificates other than the CA.
func (s *SelfSigner) mintedHostsLocked() int {
	n := len(s.store.certificates)
	if s.store.certificates[selfSignerCAMagicName] != nil {
		n--
	}
	return n
}

// GetCertificate returns the certificate for the given hostname
// returns nil if the certificate is not found
func (s *SelfSigner) GetCertificate(info *tls.ClientHelloInfo) (*tls.Certificate, error) {
	var hostname string
	// A name the client sent counts against the cap; the address the
	// connection reached is one of the device's own (D15).
	capped := true
	if info.ServerName != "" && info.ServerName != selfSignerCAMagicName {
		hostname = info.ServerName
	} else {
		hostname = strings.Split(info.Conn.LocalAddr().String(), ":")[0]
		capped = false
	}

	s.log.Info().Str("hostname", hostname).Strs("supported_protos", info.SupportedProtos).Msg("TLS handshake")

	// convert hostname to punycode
	h, err := idna.Lookup.ToASCII(hostname)
	if err != nil {
		s.log.Warn().Str("hostname", hostname).Err(err).Str("remote_addr", info.Conn.RemoteAddr().String()).Msg("Hostname is not valid")
		hostname = s.DefaultDomain
	} else {
		hostname = h
	}

	cert := s.createSelfSignedCert(hostname, capped)
	return cert, nil
}
