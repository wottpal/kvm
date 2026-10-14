package websecure

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestSigner(t *testing.T) (*SelfSigner, string) {
	t.Helper()
	dir := t.TempDir()
	store := NewCertStore(dir, nil)
	store.LoadCertificates()
	return NewSelfSigner(store, &defaultLogger, "jetkvm.local", "JetKVM", "JetKVM", "JetKVM CA"), dir
}

func leafNames(t *testing.T, cert *tls.Certificate) []string {
	t.Helper()
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatal("no certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.DNSNames
}

// Every TLS handshake asks for its SNI's certificate in its own goroutine,
// before any authentication; distinct names must not race on the store's map
// (a concurrent map write is fatal) nor mint two CAs (D15).
func TestSelfSignerConcurrentHandshakes(t *testing.T) {
	signer, _ := newTestSigner(t)
	const n = 16
	certs := make([]*tls.Certificate, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			certs[i], _ = signer.GetCertificate(&tls.ClientHelloInfo{ServerName: fmt.Sprintf("host-%d.local", i%4)})
		}(i)
	}
	wg.Wait()

	ca := signer.store.GetCertificate(selfSignerCAMagicName)
	if ca == nil {
		t.Fatal("no CA")
	}
	for i, cert := range certs {
		if cert == nil || len(cert.Certificate) != 2 || string(cert.Certificate[1]) != string(ca.Certificate[0]) {
			t.Fatalf("certificate %d is not signed by the stored CA", i)
		}
	}
}

// An unauthenticated client picks the SNI: after maxSelfSignedHosts names the
// signer answers with the default domain's certificate instead of minting
// (and storing) one per name (D15).
func TestSelfSignerCapsMintedHosts(t *testing.T) {
	signer, dir := newTestSigner(t)
	for i := 0; i < maxSelfSignedHosts+20; i++ {
		if _, err := signer.GetCertificate(&tls.ClientHelloInfo{ServerName: fmt.Sprintf("junk-%d.example", i)}); err != nil {
			t.Fatal(err)
		}
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	crts := 0
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".crt") {
			crts++
		}
	}
	// The CA, maxSelfSignedHosts names, and the default domain's certificate.
	if want := maxSelfSignedHosts + 2; crts > want {
		t.Fatalf("%d certificates stored, want at most %d", crts, want)
	}

	cert, _ := signer.GetCertificate(&tls.ClientHelloInfo{ServerName: "one-more.example"})
	if names := leafNames(t, cert); len(names) != 1 || names[0] != "jetkvm.local" {
		t.Fatalf("over the cap the certificate names %v, want the default domain", names)
	}
	// Names already issued keep their certificate.
	cert, _ = signer.GetCertificate(&tls.ClientHelloInfo{ServerName: "junk-0.example"})
	if names := leafNames(t, cert); len(names) != 1 || names[0] != "junk-0.example" {
		t.Fatalf("an issued name now gets %v", names)
	}
}

// A device that already minted certificates loads them at boot (D15 review):
// loading must not take the store's lock around loadCertificate, which takes
// it itself, or the HTTPS server never starts.
func TestReloadStoreWithCertificates(t *testing.T) {
	signer, dir := newTestSigner(t)
	if _, err := signer.GetCertificate(&tls.ClientHelloInfo{ServerName: "kvm.local"}); err != nil {
		t.Fatal(err)
	}

	done := make(chan *CertStore, 1)
	go func() {
		store := NewCertStore(dir, &defaultLogger)
		store.LoadCertificates()
		done <- store
	}()
	select {
	case store := <-done:
		if store.GetCertificate("kvm.local") == nil || store.GetCertificate(selfSignerCAMagicName) == nil {
			t.Fatal("reloaded store lacks the minted certificates")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadCertificates did not return: deadlock")
	}
}

// Junk names fill the cap; the device's own names and addresses are still
// minted (D15 review).
func TestSelfSignerCapSparesLocalNames(t *testing.T) {
	signer, _ := newTestSigner(t)
	signer.LocalNames = func() []string { return []string{"kvm-office", "kvm-office.fritz.box"} }
	for i := 0; i < maxSelfSignedHosts+5; i++ {
		signer.GetCertificate(&tls.ClientHelloInfo{ServerName: fmt.Sprintf("junk-%d.example", i)})
	}
	for _, name := range []string{"kvm-office.fritz.box", "KVM-Office"} {
		cert, _ := signer.GetCertificate(&tls.ClientHelloInfo{ServerName: name})
		if names := leafNames(t, cert); len(names) != 1 || !strings.EqualFold(names[0], name) {
			t.Fatalf("%s past the cap gets %v", name, names)
		}
	}
	// An address as SNI is the client's choice too: capped.
	cert, _ := signer.GetCertificate(&tls.ClientHelloInfo{ServerName: "10.9.8.7"})
	if names := leafNames(t, cert); len(names) != 1 || names[0] != "jetkvm.local" {
		t.Fatalf("an address sent as SNI past the cap gets %v", names)
	}
}
