package wfe

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	mrand "math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/letsencrypt/go-jose"
	"github.com/letsencrypt/boulder/cmd/load-generator/latency"
)

type registration struct {
	key    *rsa.PrivateKey
	signer jose.Signer
	iMu    *sync.RWMutex
	auths  []string
	certs  []string
}

type State struct {
	rMu      *sync.RWMutex
	regs     []*registration
	maxRegs  int
	client   *http.Client
	apiBase  string
	termsURL string

	nMu       *sync.RWMutex
	noncePool []string

	throughput int64

	challRPCAddr string

	certKey    *rsa.PrivateKey
	domainBase string

	callLatency *latency.Map

	runtime time.Duration

	challSrvProc *os.Process

	wg *sync.WaitGroup
}

type rawRegistration struct {
	Certs  []string `json:"certs"`
	Auths  []string `json:"auths"`
	RawKey []byte   `json:"rawKey"`
}

type snapshot struct {
	Registrations []rawRegistration
}

func (s *State) Snapshot() ([]byte, error) {
	s.rMu.Lock()
	defer s.rMu.Unlock()
	snap := snapshot{}
	rawRegs := []rawRegistration{}
	for _, r := range s.regs {
		rawRegs = append(rawRegs, rawRegistration{
			Certs:  r.certs,
			Auths:  r.auths,
			RawKey: x509.MarshalPKCS1PrivateKey(r.key),
		})
	}
	return json.Marshal(snap)
}

func (s *State) Restore(content []byte) error {
	s.rMu.Lock()
	defer s.rMu.Unlock()
	snap := snapshot{}
	err := json.Unmarshal(content, &snap)
	if err != nil {
		return err
	}
	for _, r := range snap.Registrations {
		key, err := x509.ParsePKCS1PrivateKey(r.RawKey)
		if err != nil {
			continue
		}
		signer, err := jose.NewSigner(jose.RS256, key)
		if err != nil {
			continue
		}
		s.regs = append(s.regs, &registration{
			key:    key,
			signer: signer,
			certs:  r.Certs,
			auths:  r.Auths,
		})
	}
	return nil
}

func New(rpcAddr string, apiBase string, rate int, keySize int, domainBase string, runtime time.Duration, termsURL string) (*State, error) {
	certKey, err := rsa.GenerateKey(rand.Reader, keySize)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Transport: &http.Transport{
			Dial: (&net.Dialer{
				Timeout:   3 * time.Second,
				KeepAlive: 0,
			}).Dial,
			TLSHandshakeTimeout: 2 * time.Second,
			DisableKeepAlives:   true,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}
	return &State{
		rMu:          new(sync.RWMutex),
		nMu:          new(sync.RWMutex),
		challRPCAddr: rpcAddr,
		client:       client,
		apiBase:      apiBase,
		throughput:   int64(rate),
		certKey:      certKey,
		domainBase:   domainBase,
		callLatency:  latency.New(fmt.Sprintf("WFE -- %s test at %d base actions / second", runtime, rate)),
		runtime:      runtime,
		termsURL:     termsURL,
		wg:           new(sync.WaitGroup),
	}, nil
}

func (s *State) Run(binName string, dontRunChallSrv bool, httpOneAddr string) error {
	// Start chall server process
	if !dontRunChallSrv {
		cmd := exec.Command(binName, "chall-srv", "--rpcAddr="+s.challRPCAddr, "--httpOneAddr="+httpOneAddr)
		err := cmd.Start()
		if err != nil {
			return err
		}
		s.challSrvProc = cmd.Process
	}

	// Run sending loop
	stop := make(chan bool, 1)
	s.callLatency.Started = time.Now()

	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				s.wg.Add(1)
				go s.sendCall()
				time.Sleep(time.Duration(time.Second.Nanoseconds() / atomic.LoadInt64(&s.throughput)))
			}
		}
	}()

	time.Sleep(s.runtime)
	fmt.Println("READ END")
	stop <- true
	fmt.Println("SENT STOP")
	s.wg.Wait()
	fmt.Println("KILLING CHALL SERVER")
	err := s.challSrvProc.Kill()
	if err != nil {
		fmt.Printf("Error killing challenge server: %s\n", err)
	}
	fmt.Println("ALL DONE")
	s.callLatency.Stopped = time.Now()
	return nil
}

func (s *State) Dump(jsonPath string) error {
	if jsonPath != "" {
		data, err := json.Marshal(s.callLatency)
		if err != nil {
			return err
		}
		err = ioutil.WriteFile(jsonPath, data, os.ModePerm)
		if err != nil {
			return err
		}
	}
	return nil
}

// HTTP utils

func (s *State) post(endpoint string, payload []byte) (*http.Response, error) {
	resp, err := s.client.Post(
		endpoint,
		"application/json",
		bytes.NewBuffer(payload),
	)
	if resp != nil {
		if newNonce := resp.Header.Get("Replay-Nonce"); newNonce != "" {
			s.addNonce(newNonce)
		}
	}
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Nonce utils, these methods are used to generate/store/retrieve the nonces
// required for the required form of JWS

func (s *State) signWithNonce(endpoint string, alwaysNew bool, payload []byte, signer jose.Signer) ([]byte, error) {
	nonce, err := s.getNonce(endpoint, alwaysNew)
	if err != nil {
		return nil, err
	}
	jws, err := signer.Sign(payload, nonce)
	if err != nil {
		return nil, err
	}
	return json.Marshal(jws)
}

func (s *State) getNonce(from string, alwaysNew bool) (string, error) {
	s.nMu.RLock()
	if len(s.noncePool) == 0 || alwaysNew {
		s.nMu.RUnlock()
		started := time.Now()
		resp, err := s.client.Head(fmt.Sprintf("%s%s", s.apiBase, from))
		finished := time.Now()
		state := "good"
		defer func() { s.callLatency.Add(fmt.Sprintf("HEAD %s", from), started, finished, state) }()
		if err != nil {
			state = "error"
			return "", err
		}
		if nonce := resp.Header.Get("Replay-Nonce"); nonce != "" {
			return nonce, nil
		}
		state = "error"
		return "", fmt.Errorf("Nonce header not supplied!")
	}
	s.nMu.RUnlock()
	s.nMu.Lock()
	defer s.nMu.Unlock()
	nonce := s.noncePool[0]
	s.noncePool = s.noncePool[1:]
	return nonce, nil
}

func (s *State) addNonce(nonce string) {
	s.nMu.Lock()
	defer s.nMu.Unlock()
	s.noncePool = append(s.noncePool, nonce)
}

// Reg object utils, used to add and randomly retrieve registration objects

func (s *State) addReg(reg *registration) {
	s.rMu.Lock()
	defer s.rMu.Unlock()
	s.regs = append(s.regs, reg)
}

func (s *State) getRandReg() (*registration, bool) {
	regsLength := len(s.regs)
	if regsLength == 0 {
		return nil, false
	}
	return s.regs[mrand.Intn(regsLength)], true
}

func (s *State) getReg() (*registration, bool) {
	s.rMu.RLock()
	defer s.rMu.RUnlock()
	return s.getRandReg()
}

// Call sender, it sends the calls!

type probabilityProfile struct {
	prob   int
	action func(*registration)
}

func weightedCall(setup []probabilityProfile) func(*registration) {
	choices := make(map[int]func(*registration))
	n := 0
	for _, pp := range setup {
		for i := 0; i < pp.prob; i++ {
			choices[i+n] = pp.action
		}
		n += pp.prob
	}
	if len(choices) == 0 {
		return nil
	}

	return choices[mrand.Intn(n)]
}

func (s *State) sendCall() {
	actionList := []probabilityProfile{probabilityProfile{2, s.newRegistration}}

	reg, found := s.getReg()
	if found {
		actionList = append(actionList, probabilityProfile{4, s.newAuthorization})
		reg.iMu.RLock()
		if len(reg.auths) > 0 {
			actionList = append(actionList, probabilityProfile{4, s.newCertificate})
		}
		if len(reg.certs) > 0 {
			actionList = append(actionList, probabilityProfile{3, s.revokeCertificate})
		}
		reg.iMu.RUnlock()
	}

	weightedCall(actionList)(reg)
	s.wg.Done()
}