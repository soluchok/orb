/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package writer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/hyperledger/aries-framework-go/component/storageutil/mem"
	mockstore "github.com/hyperledger/aries-framework-go/component/storageutil/mock"
	"github.com/hyperledger/aries-framework-go/pkg/doc/signature/verifier"
	"github.com/hyperledger/aries-framework-go/pkg/doc/verifiable"
	"github.com/stretchr/testify/require"
	"github.com/trustbloc/sidetree-core-go/pkg/api/operation"
	"github.com/trustbloc/sidetree-core-go/pkg/api/protocol"

	"github.com/trustbloc/orb/pkg/activitypub/client/transport"
	"github.com/trustbloc/orb/pkg/activitypub/store/memstore"
	apmocks "github.com/trustbloc/orb/pkg/activitypub/store/mocks"
	"github.com/trustbloc/orb/pkg/activitypub/store/spi"
	"github.com/trustbloc/orb/pkg/activitypub/vocab"
	"github.com/trustbloc/orb/pkg/anchor/graph"
	anchormocks "github.com/trustbloc/orb/pkg/anchor/mocks"
	"github.com/trustbloc/orb/pkg/anchor/proof"
	"github.com/trustbloc/orb/pkg/anchor/subject"
	"github.com/trustbloc/orb/pkg/cas/ipfs"
	casresolver "github.com/trustbloc/orb/pkg/cas/resolver"
	"github.com/trustbloc/orb/pkg/didanchor/memdidanchor"
	discoveryrest "github.com/trustbloc/orb/pkg/discovery/endpoint/restapi"
	orberrors "github.com/trustbloc/orb/pkg/errors"
	"github.com/trustbloc/orb/pkg/internal/testutil"
	"github.com/trustbloc/orb/pkg/mocks"
	"github.com/trustbloc/orb/pkg/pubsub/mempubsub"
	resourceresolver "github.com/trustbloc/orb/pkg/resolver/resource"
	"github.com/trustbloc/orb/pkg/store/cas"
	"github.com/trustbloc/orb/pkg/store/vcstatus"
	vcstore "github.com/trustbloc/orb/pkg/store/verifiable"
	"github.com/trustbloc/orb/pkg/vcsigner"
	wfclient "github.com/trustbloc/orb/pkg/webfinger/client"
)

const (
	namespace = "did:sidetree"

	testDID = "did:method:abc"

	activityPubURL = "http://localhost/services/orb"
	casURL         = "http://localhost/cas"

	testMaxWitnessDelay = 600 * time.Second

	signWithLocalWitness = true

	webfingerPayload = `{"properties":{"https://trustbloc.dev/ns/ledger-type":"vct-v1"}}`
)

func TestNew(t *testing.T) {
	apServiceIRI, err := url.Parse(activityPubURL)
	require.NoError(t, err)

	casIRI, err := url.Parse(casURL)
	require.NoError(t, err)

	vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
	require.NoError(t, err)

	providers := &Providers{
		AnchorGraph:   graph.New(&graph.Providers{}),
		DidAnchors:    memdidanchor.New(),
		AnchorBuilder: &mockTxnBuilder{},
		VCStore:       vcStore,
	}

	t.Run("Success", func(t *testing.T) {
		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, &mocks.PubSub{},
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)
		require.NotNil(t, c)
	})

	t.Run("Error", func(t *testing.T) {
		errExpected := errors.New("injected subscribe error")

		ps := &mocks.PubSub{}
		ps.SubscribeReturns(nil, errExpected)

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.Error(t, err)
		require.Contains(t, err.Error(), errExpected.Error())
		require.Nil(t, c)
	})
}

func TestWriter_WriteAnchor(t *testing.T) {
	ps := mempubsub.New(mempubsub.Config{})
	defer ps.Stop()

	casClient, err := cas.New(mem.NewProvider(), casURL, nil, &mocks.MetricsProvider{}, 100)

	require.NoError(t, err)

	graphProviders := &graph.Providers{
		CasWriter: casClient,
		CasResolver: casresolver.New(casClient, nil,
			casresolver.NewWebCASResolver(
				transport.New(&http.Client{}, testutil.MustParseURL("https://example.com/keys/public-key"),
					transport.DefaultSigner(), transport.DefaultSigner()),
				wfclient.New(), "https"), &mocks.MetricsProvider{}),
		Pkf: pubKeyFetcherFnc,
	}

	apServiceIRI, err := url.Parse(activityPubURL)
	require.NoError(t, err)

	casIRI, err := url.Parse(casURL)
	require.NoError(t, err)

	anchorGraph := graph.New(graphProviders)

	wfHTTPClient := httpMock(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			Body:       ioutil.NopCloser(bytes.NewBufferString(webfingerPayload)),
			StatusCode: http.StatusOK,
		}, nil
	})

	wfClient := wfclient.New(wfclient.WithHTTPClient(wfHTTPClient))

	t.Run("success - no local witness configured, "+
		"witness needs to be resolved via HTTP", func(t *testing.T) {
		vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
		require.NoError(t, err)

		vcStatusStore, err := vcstatus.New(mem.NewProvider())
		require.NoError(t, err)

		providers := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			OpProcessor:   &mockOpProcessor{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			MonitoringSvc: &mockMonitoring{},
			WitnessStore:  &mockWitnessStore{},
			ActivityStore: &mockActivityStore{},
			VCStore:       vcStore,
			VCStatusStore: vcStatusStore,
			WFClient:      wfClient,
		}

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, false, testutil.GetLoader(t), resourceresolver.New(http.DefaultClient,
				nil), &mocks.MetricsProvider{})
		require.NoError(t, err)

		var testServerURL string

		testServer := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err = w.Write(generateValidExampleHostMetaResponse(t, testServerURL))
				require.NoError(t, err)
			}))
		defer testServer.Close()

		testServerURL = testServer.URL

		opRefs := []*operation.Reference{
			{
				UniqueSuffix: "did-1",
				Type:         operation.TypeCreate,
				AnchorOrigin: fmt.Sprintf("%s/services/orb", testServerURL),
			},
		}

		err = c.WriteAnchor("1.anchor", nil, opRefs, 1)
		require.NoError(t, err)
	})

	t.Run("success - witness needs to be resolved via IPNS", func(t *testing.T) {
		vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
		require.NoError(t, err)

		vcStatusStore, err := vcstatus.New(mem.NewProvider())
		require.NoError(t, err)

		providers := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			OpProcessor:   &mockOpProcessor{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			MonitoringSvc: &mockMonitoring{},
			WitnessStore:  &mockWitnessStore{},
			ActivityStore: &mockActivityStore{},
			VCStore:       vcStore,
			VCStatusStore: vcStatusStore,
			WFClient:      wfClient,
		}

		var testServerURL string

		testServer := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err = w.Write(generateValidExampleHostMetaResponse(t, testServerURL))
				require.NoError(t, err)
			}))
		defer testServer.Close()

		testServerURL = testServer.URL

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, false, testutil.GetLoader(t),
			resourceresolver.New(http.DefaultClient,
				ipfs.New(testServer.URL, 5*time.Second, 0, &mocks.MetricsProvider{}),
			), &mocks.MetricsProvider{})
		require.NoError(t, err)

		opRefs := []*operation.Reference{
			{
				UniqueSuffix: "did-1",
				Type:         operation.TypeCreate,
				AnchorOrigin: "ipns://k51qzi5uqu5dgjceyz40t6xfnae8jqn5z17ojojggzwz2mhl7uyhdre8ateqek",
			},
		}

		err = c.WriteAnchor("1.anchor", nil, opRefs, 1)
		require.NoError(t, err)
	})

	t.Run("success - local witness configured, sign with default witness is false", func(t *testing.T) {
		vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
		require.NoError(t, err)

		vcStatusStore, err := vcstatus.New(mem.NewProvider())
		require.NoError(t, err)

		providers := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			OpProcessor:   &mockOpProcessor{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			MonitoringSvc: &mockMonitoring{},
			Witness:       &mockWitness{},
			WitnessStore:  &mockWitnessStore{},
			ActivityStore: &mockActivityStore{},
			VCStore:       vcStore,
			VCStatusStore: vcStatusStore,
			WFClient:      wfClient,
		}

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, false, testutil.GetLoader(t),
			resourceresolver.New(http.DefaultClient, nil), &mocks.MetricsProvider{})
		require.NoError(t, err)

		var testServerURL string

		testServer := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err = w.Write(generateValidExampleHostMetaResponse(t, testServerURL))
				require.NoError(t, err)
			}))
		defer testServer.Close()

		testServerURL = testServer.URL

		opRefs := []*operation.Reference{
			{
				UniqueSuffix: "did-1",
				Type:         operation.TypeCreate,
				AnchorOrigin: fmt.Sprintf("%s/services/orb", testServerURL),
			},
		}

		err = c.WriteAnchor("1.anchor", nil, opRefs, 1)
		require.NoError(t, err)
	})

	t.Run("success - local witness", func(t *testing.T) {
		vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
		require.NoError(t, err)

		wit := &mockWitness{proofBytes: []byte(`{"proof": {"domain":"domain","created": "2021-02-23T19:36:07Z"}}`)}

		vcStatusStore, err := vcstatus.New(mem.NewProvider())
		require.NoError(t, err)

		providers := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			OpProcessor:   &mockOpProcessor{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			Witness:       wit,
			MonitoringSvc: &mockMonitoring{},
			WitnessStore:  &mockWitnessStore{},
			ActivityStore: &mockActivityStore{},
			VCStore:       vcStore,
			VCStatusStore: vcStatusStore,
			WFClient:      wfClient,
		}

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t),
			resourceresolver.New(http.DefaultClient, nil), &mocks.MetricsProvider{})
		require.NoError(t, err)

		var testServerURL string

		testServer := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err = w.Write(generateValidExampleHostMetaResponse(t, testServerURL))
				require.NoError(t, err)
			}))
		defer testServer.Close()

		testServerURL = testServer.URL

		opRefs := []*operation.Reference{
			{
				UniqueSuffix: "did-1",
				Type:         operation.TypeCreate,
				AnchorOrigin: fmt.Sprintf("%s/services/orb", testServerURL),
			},
		}

		err = c.WriteAnchor("1.anchor", nil, opRefs, 1)
		require.NoError(t, err)
	})

	t.Run("error - vc status store error", func(t *testing.T) {
		vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
		require.NoError(t, err)

		wit := &mockWitness{proofBytes: []byte(`{"proof": {"domain":"domain","created": "2021-02-23T19:36:07Z"}}`)}

		providers := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			OpProcessor:   &mockOpProcessor{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			Witness:       wit,
			MonitoringSvc: &mockMonitoring{},
			WitnessStore:  &mockWitnessStore{},
			ActivityStore: &mockActivityStore{},
			VCStore:       vcStore,
			VCStatusStore: &mockVCStatusStore{Err: fmt.Errorf("vc status error")},
			WFClient:      wfClient,
		}

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t),
			resourceresolver.New(http.DefaultClient, nil), &mocks.MetricsProvider{})
		require.NoError(t, err)

		var testServerURL string

		testServer := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err = w.Write(generateValidExampleHostMetaResponse(t, testServerURL))
				require.NoError(t, err)
			}))
		defer testServer.Close()

		testServerURL = testServer.URL

		opRefs := []*operation.Reference{
			{
				UniqueSuffix: "did-1",
				Type:         operation.TypeCreate,
				AnchorOrigin: fmt.Sprintf("%s/services/orb", testServerURL),
			},
		}

		err = c.WriteAnchor("1.anchor", nil, opRefs, 1)
		require.Error(t, err)
		require.Contains(t, err.Error(),
			"failed to set 'in-process' status for vcID[http://domain.com/vc/123]: vc status error")
	})

	t.Run("Parse created time (error)", func(t *testing.T) {
		vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
		require.NoError(t, err)

		wit := &mockWitness{proofBytes: []byte(`{"proof": {"created": "021-02-23T:07Z"}}`)}

		providers := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			OpProcessor:   &mockOpProcessor{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			Witness:       wit,
			MonitoringSvc: &mockMonitoring{},
			WitnessStore:  &mockWitnessStore{},
			ActivityStore: &mockActivityStore{},
			VCStore:       vcStore,
			WFClient:      wfClient,
		}

		c, err := New(namespace, apServiceIRI, casIRI, providers, nil, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t),
			resourceresolver.New(http.DefaultClient, nil), &mocks.MetricsProvider{})
		require.NoError(t, err)

		var testServerURL string

		testServer := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err = w.Write(generateValidExampleHostMetaResponse(t, testServerURL))
				require.NoError(t, err)
			}))
		defer testServer.Close()

		testServerURL = testServer.URL

		opRefs := []*operation.Reference{
			{
				UniqueSuffix: "did-1",
				Type:         operation.TypeCreate,
				AnchorOrigin: fmt.Sprintf("%s/services/orb", testServerURL),
			},
		}

		err = c.WriteAnchor("1.anchor", nil, opRefs, 1)
		require.Error(t, err)
		require.Contains(t, err.Error(), "parse created: parsing time")
	})

	t.Run("error - failed to get witness list", func(t *testing.T) {
		vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
		require.NoError(t, err)

		providers := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    &mockDidAnchor{},
			AnchorBuilder: &mockTxnBuilder{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			MonitoringSvc: &mockMonitoring{},
			WitnessStore:  &mockWitnessStore{},
			ActivityStore: &mockActivityStore{},
			VCStore:       vcStore,
			OpProcessor:   &mockOpProcessor{Err: errors.New("operation processor error")},
			WFClient:      wfClient,
		}

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		opRefs := []*operation.Reference{
			{
				UniqueSuffix: "did-1",
				Type:         operation.TypeUpdate,
				AnchorOrigin: "origin.com",
			},
		}

		err = c.WriteAnchor("1.anchor", nil, opRefs, 1)
		require.Error(t, err)
		require.Equal(t, err.Error(), "failed to create witness list: operation processor error")
	})

	t.Run("error - build anchor credential error", func(t *testing.T) {
		providersWithErr := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{Err: errors.New("sign error")},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
		}

		c, err := New(namespace, apServiceIRI, casIRI, providersWithErr, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		err = c.WriteAnchor("1.anchor", nil, []*operation.Reference{{UniqueSuffix: testDID, Type: operation.TypeCreate}}, 1)
		require.Contains(t, err.Error(), "failed to build anchor credential: sign error")
	})

	t.Run("error - anchor credential signing error", func(t *testing.T) {
		providersWithErr := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{Err: fmt.Errorf("signer error")},
		}

		c, err := New(namespace, apServiceIRI, casIRI, providersWithErr, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t),
			resourceresolver.New(http.DefaultClient, nil), &mocks.MetricsProvider{})
		require.NoError(t, err)

		var testServerURL string

		testServer := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err = w.Write(generateValidExampleHostMetaResponse(t, testServerURL))
				require.NoError(t, err)
			}))
		defer testServer.Close()

		testServerURL = testServer.URL

		err = c.WriteAnchor("1.anchor", nil,
			getOperationReferences(fmt.Sprintf("%s/services/orb", testServerURL)), 1)

		require.Contains(t, err.Error(), "failed to sign anchor credential[http://domain.com/vc/123]: signer error")
	})

	t.Run("error - local witness (monitoring error)", func(t *testing.T) {
		vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
		require.NoError(t, err)

		providersWithErr := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			OpProcessor:   &mockOpProcessor{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			Witness: &mockWitness{
				proofBytes: []byte(`{"proof": {"domain":"domain","created": "2021-02-23T19:36:07Z"}}`),
			},
			VCStatusStore: &mockVCStatusStore{},
			WitnessStore:  &mockWitnessStore{},
			ActivityStore: &mockActivityStore{},
			MonitoringSvc: &mockMonitoring{Err: fmt.Errorf("monitoring error")},
			VCStore:       vcStore,
		}

		c, err := New(namespace, apServiceIRI, casIRI, providersWithErr, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t),
			resourceresolver.New(http.DefaultClient, nil), &mocks.MetricsProvider{})
		require.NoError(t, err)

		var testServerURL string

		testServer := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err = w.Write(generateValidExampleHostMetaResponse(t, testServerURL))
				require.NoError(t, err)
			}))
		defer testServer.Close()

		testServerURL = testServer.URL

		err = c.WriteAnchor("1.anchor", nil,
			getOperationReferences(fmt.Sprintf("%s/services/orb", testServerURL)), 1)
		require.Error(t, err)
		require.Contains(t, err.Error(),
			"failed to setup monitoring for local witness for anchor credential[http://domain.com/vc/123]: monitoring error")
	})

	t.Run("error - local witness log error", func(t *testing.T) {
		providersWithErr := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			Witness:       &mockWitness{Err: fmt.Errorf("witness error")},
		}

		c, err := New(namespace, apServiceIRI, casIRI, providersWithErr, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t),
			resourceresolver.New(http.DefaultClient, nil), &mocks.MetricsProvider{})
		require.NoError(t, err)

		var testServerURL string

		testServer := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err = w.Write(generateValidExampleHostMetaResponse(t, testServerURL))
				require.NoError(t, err)
			}))
		defer testServer.Close()

		testServerURL = testServer.URL

		err = c.WriteAnchor("1.anchor", nil,
			getOperationReferences(fmt.Sprintf("%s/services/orb", testServerURL)), 1)
		require.Contains(t, err.Error(),
			"local witnessing failed for anchor credential[http://domain.com/vc/123]: witness error")
	})

	t.Run("error - store anchor credential error", func(t *testing.T) {
		storeProviderWithErr := &mockstore.Provider{
			OpenStoreReturn: &mockstore.Store{ErrPut: fmt.Errorf("error put")},
		}

		vcStoreWithErr, err := vcstore.New(storeProviderWithErr, testutil.GetLoader(t))
		require.NoError(t, err)

		providersWithErr := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			VCStore:       vcStoreWithErr,
		}

		c, err := New(namespace, apServiceIRI, casIRI, providersWithErr, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t),
			resourceresolver.New(http.DefaultClient, nil), &mocks.MetricsProvider{})
		require.NoError(t, err)

		var testServerURL string

		testServer := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err = w.Write(generateValidExampleHostMetaResponse(t, testServerURL))
				require.NoError(t, err)
			}))
		defer testServer.Close()

		testServerURL = testServer.URL

		err = c.WriteAnchor("1.anchor", nil,
			getOperationReferences(fmt.Sprintf("%s/services/orb", testServerURL)), 1)
		require.Error(t, err)
		require.Contains(t, err.Error(), "error put")
	})

	t.Run("error - store anchor credential error (local witness)", func(t *testing.T) {
		storeProviderWithErr := &mockstore.Provider{
			OpenStoreReturn: &mockstore.Store{ErrPut: fmt.Errorf("error put (local witness)")},
		}

		vcStoreWithErr, err := vcstore.New(storeProviderWithErr, testutil.GetLoader(t))
		require.NoError(t, err)

		providersWithErr := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			Witness:       &mockWitness{},
			MonitoringSvc: &mockMonitoring{},
			VCStore:       vcStoreWithErr,
		}

		c, err := New(namespace, apServiceIRI, casIRI, providersWithErr, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t),
			resourceresolver.New(http.DefaultClient, nil), &mocks.MetricsProvider{})
		require.NoError(t, err)

		var testServerURL string

		testServer := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err = w.Write(generateValidExampleHostMetaResponse(t, testServerURL))
				require.NoError(t, err)
			}))
		defer testServer.Close()

		testServerURL = testServer.URL

		err = c.WriteAnchor("1.anchor", nil,
			getOperationReferences(fmt.Sprintf("%s/services/orb", testServerURL)), 1)
		require.Error(t, err)
		require.Contains(t, err.Error(), "error put (local witness)")
	})

	t.Run("error - previous did anchor reference not found for non-create operations", func(t *testing.T) {
		vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
		require.NoError(t, err)

		providers := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			VCStore:       vcStore,
		}

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		err = c.WriteAnchor("anchor", nil, []*operation.Reference{{UniqueSuffix: testDID, Type: operation.TypeUpdate}}, 1)
		require.Contains(t, err.Error(),
			"previous did anchor reference not found for update operation for did[did:method:abc]")
	})

	t.Run("error - publish anchor", func(t *testing.T) {
		vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
		require.NoError(t, err)

		vcStatusStore, err := vcstatus.New(mem.NewProvider())
		require.NoError(t, err)

		providers := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			OpProcessor:   &mockOpProcessor{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			MonitoringSvc: &mockMonitoring{},
			WitnessStore:  &mockWitnessStore{},
			ActivityStore: &mockActivityStore{},
			VCStore:       vcStore,
			VCStatusStore: vcStatusStore,
			WFClient:      wfClient,
		}

		publisher := &anchormocks.AnchorPublisher{}
		publisher.PublishAnchorReturns(errors.New("injected publisher error"))

		c, err := New(namespace, apServiceIRI, casIRI, providers, publisher, ps,
			testMaxWitnessDelay, false, testutil.GetLoader(t),
			resourceresolver.New(http.DefaultClient, nil), &mocks.MetricsProvider{})
		require.NoError(t, err)

		var testServerURL string

		testServer := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err = w.Write(generateValidExampleHostMetaResponse(t, testServerURL))
				require.NoError(t, err)
			}))
		defer testServer.Close()

		testServerURL = testServer.URL

		opRefs := []*operation.Reference{
			{
				UniqueSuffix: "did-1",
				Type:         operation.TypeCreate,
				AnchorOrigin: fmt.Sprintf("%s/services/orb", testServerURL),
			},
		}

		err = c.WriteAnchor("1.anchor", nil, opRefs, 1)
		require.NoError(t, err)
	})

	t.Run("error - fail to resolve anchor origin via IPNS (IPFS node not reachable)", func(t *testing.T) {
		vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
		require.NoError(t, err)

		vcStatusStore, err := vcstatus.New(mem.NewProvider())
		require.NoError(t, err)

		providers := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			OpProcessor:   &mockOpProcessor{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			MonitoringSvc: &mockMonitoring{},
			WitnessStore:  &mockWitnessStore{},
			ActivityStore: &mockActivityStore{},
			VCStore:       vcStore,
			VCStatusStore: vcStatusStore,
		}

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, false, testutil.GetLoader(t),
			resourceresolver.New(nil, ipfs.New("SomeIPFSNodeURL", time.Second, 0, &mocks.MetricsProvider{})),
			&mocks.MetricsProvider{})
		require.NoError(t, err)

		opRefs := []*operation.Reference{
			{
				UniqueSuffix: "did-4",
				Type:         operation.TypeCreate,
				AnchorOrigin: "ipns://k51qzi5uqu5dgjceyz40t6xfnae8jqn5z17ojojggzwz2mhl7uyhdre8ateqek",
			},
		}

		err = c.WriteAnchor("1.anchor", nil, opRefs, 1)
		require.Error(t, err)
		require.Contains(t, err.Error(), `failed to get host-meta document via IPNS: failed to read from IPNS: `+
			`Post "http://SomeIPFSNodeURL/api/v0/cat?arg=%2Fipns%2Fk51qzi5uqu5dgjceyz40t6xfnae8jqn5z17ojojggzwz2mh`+
			`l7uyhdre8ateqek%2F.well-known%2Fhost-meta.json":`)
	})

	t.Run("error - no witnesses configured", func(t *testing.T) {
		vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
		require.NoError(t, err)

		vcStatusStore, err := vcstatus.New(mem.NewProvider())
		require.NoError(t, err)

		activityStore := memstore.New("")

		providers := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			OpProcessor:   &mockOpProcessor{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			MonitoringSvc: &mockMonitoring{},
			WitnessStore:  &mockWitnessStore{},
			ActivityStore: activityStore,
			VCStore:       vcStore,
			VCStatusStore: vcStatusStore,
			WFClient:      wfClient,
		}

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, false, testutil.GetLoader(t), resourceresolver.New(http.DefaultClient,
				nil), &mocks.MetricsProvider{})
		require.NoError(t, err)

		hostMetaResponse := discoveryrest.JRD{
			Subject:    "",
			Properties: nil,
			Links: []discoveryrest.Link{
				{
					Type: discoveryrest.ActivityJSONType,
					Href: apServiceIRI.String(),
				},
			},
		}

		hostMetaResponseBytes, err := json.Marshal(hostMetaResponse)
		require.NoError(t, err)

		testServer := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err = w.Write(hostMetaResponseBytes)
				require.NoError(t, err)
			}))
		defer testServer.Close()

		opRefs := []*operation.Reference{
			{
				UniqueSuffix: "did-1",
				Type:         operation.TypeCreate,
				AnchorOrigin: fmt.Sprintf("%s/services/orb", testServer.URL),
			},
		}

		err = c.WriteAnchor("1.anchor", nil, opRefs, 1)
		require.Error(t, err)
		require.True(t, orberrors.IsTransient(err))
		require.Contains(t, err.Error(), "no witnesses are provided")
	})
}

func TestWriter_handle(t *testing.T) {
	ps := mempubsub.New(mempubsub.Config{})
	defer ps.Stop()

	casClient, err := cas.New(mem.NewProvider(), casURL, nil, &mocks.MetricsProvider{}, 0)

	require.NoError(t, err)

	graphProviders := &graph.Providers{
		CasWriter: casClient,
		CasResolver: casresolver.New(casClient, nil,
			casresolver.NewWebCASResolver(
				transport.New(&http.Client{}, testutil.MustParseURL("https://example.com/keys/public-key"),
					transport.DefaultSigner(), transport.DefaultSigner()),
				wfclient.New(), "https"), &mocks.MetricsProvider{}),
		Pkf: pubKeyFetcherFnc,
	}

	apServiceIRI, err := url.Parse(activityPubURL)
	require.NoError(t, err)

	casIRI, err := url.Parse(casURL)
	require.NoError(t, err)

	anchorGraph := graph.New(graphProviders)

	t.Run("success", func(t *testing.T) {
		vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
		require.NoError(t, err)

		providers := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			VCStore:       vcStore,
			WitnessStore:  &mockWitnessStore{},
		}

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		anchorVC, err := verifiable.ParseCredential([]byte(anchorCred),
			verifiable.WithDisabledProofCheck(),
			verifiable.WithJSONLDDocumentLoader(testutil.GetLoader(t)),
		)
		require.NoError(t, err)

		require.NoError(t, c.handle(anchorVC))
	})

	t.Run("error - save anchor credential to store error", func(t *testing.T) {
		storeProviderWithErr := &mockstore.Provider{
			OpenStoreReturn: &mockstore.Store{ErrPut: fmt.Errorf("error put")},
		}

		vcStoreWithErr, err := vcstore.New(storeProviderWithErr, testutil.GetLoader(t))
		require.NoError(t, err)

		providersWithErr := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			VCStore:       vcStoreWithErr,
		}

		c, err := New(namespace, apServiceIRI, casIRI, providersWithErr, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		anchorVC, err := verifiable.ParseCredential([]byte(anchorCred),
			verifiable.WithDisabledProofCheck(),
			verifiable.WithJSONLDDocumentLoader(testutil.GetLoader(t)),
		)
		require.NoError(t, err)

		err = c.handle(anchorVC)
		require.Error(t, err)
		require.Contains(t, err.Error(), "store witnessed anchor credential")
		require.True(t, orberrors.IsTransient(err))
	})

	t.Run("error - add anchor credential to txn graph error", func(t *testing.T) {
		vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
		require.NoError(t, err)

		providersWithErr := &Providers{
			AnchorGraph:   &mockAnchorGraph{Err: errors.New("txn graph error")},
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			VCStore:       vcStore,
		}

		c, err := New(namespace, apServiceIRI, casIRI, providersWithErr, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		anchorVC, err := verifiable.ParseCredential([]byte(anchorCred),
			verifiable.WithDisabledProofCheck(),
			verifiable.WithJSONLDDocumentLoader(testutil.GetLoader(t)),
		)
		require.NoError(t, err)

		err = c.handle(anchorVC)
		require.Error(t, err)
		require.Contains(t, err.Error(), "add witnessed anchor credential")
	})

	t.Run("error - add anchor credential cid to did anchors error", func(t *testing.T) {
		vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
		require.NoError(t, err)

		providersWithErr := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    &mockDidAnchor{Err: errors.New("did references error")},
			AnchorBuilder: &mockTxnBuilder{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			VCStore:       vcStore,
			WitnessStore:  &mockWitnessStore{},
		}

		errExpected := errors.New("anchor publisher error")

		anchorPublisher := &anchormocks.AnchorPublisher{}
		anchorPublisher.PublishAnchorReturns(errExpected)

		c, err := New(namespace, apServiceIRI, casIRI, providersWithErr, anchorPublisher, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		anchorVC, err := verifiable.ParseCredential([]byte(anchorCred),
			verifiable.WithDisabledProofCheck(),
			verifiable.WithJSONLDDocumentLoader(testutil.GetLoader(t)),
		)
		require.NoError(t, err)

		err = c.handle(anchorVC)
		require.Error(t, err)
		require.Contains(t, err.Error(), errExpected.Error())
	})

	t.Run("error - outbox error", func(t *testing.T) {
		vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
		require.NoError(t, err)

		providers := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			Outbox:        &mockOutbox{Err: errors.New("outbox error")},
			VCStore:       vcStore,
		}

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		anchorVC, err := verifiable.ParseCredential([]byte(anchorCred),
			verifiable.WithDisabledProofCheck(),
			verifiable.WithJSONLDDocumentLoader(testutil.GetLoader(t)),
		)
		require.NoError(t, err)

		err = c.handle(anchorVC)
		require.Error(t, err)
		require.Contains(t, err.Error(), "post create activity for hl")
		require.False(t, orberrors.IsTransient(err))
	})

	t.Run("error - delete transient data from witness store error", func(t *testing.T) {
		vcStore, err := vcstore.New(mem.NewProvider(), testutil.GetLoader(t))
		require.NoError(t, err)

		providers := &Providers{
			AnchorGraph:   anchorGraph,
			DidAnchors:    memdidanchor.New(),
			AnchorBuilder: &mockTxnBuilder{},
			Outbox:        &mockOutbox{},
			Signer:        &mockSigner{},
			VCStore:       vcStore,
			WitnessStore:  &mockWitnessStore{DeleteErr: fmt.Errorf("delete error")},
		}

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		anchorVC, err := verifiable.ParseCredential([]byte(anchorCred),
			verifiable.WithDisabledProofCheck(),
			verifiable.WithJSONLDDocumentLoader(testutil.GetLoader(t)),
		)
		require.NoError(t, err)

		require.NoError(t, c.handle(anchorVC))
	})
}

func TestWriter_postOfferActivity(t *testing.T) {
	ps := mempubsub.New(mempubsub.Config{})
	defer ps.Stop()

	apServiceIRI, err := url.Parse(activityPubURL)
	require.NoError(t, err)

	casIRI, err := url.Parse(casURL)
	require.NoError(t, err)

	wfHTTPClient := httpMock(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			Body:       ioutil.NopCloser(bytes.NewBufferString(webfingerPayload)),
			StatusCode: http.StatusOK,
		}, nil
	})

	wfClient := wfclient.New(wfclient.WithHTTPClient(wfHTTPClient))

	t.Run("success", func(t *testing.T) {
		vcStatusStore, err := vcstatus.New(mem.NewProvider())
		require.NoError(t, err)

		providers := &Providers{
			Outbox:        &mockOutbox{},
			WitnessStore:  &mockWitnessStore{},
			ActivityStore: &mockActivityStore{},
			VCStatusStore: vcStatusStore,
			WFClient:      wfClient,
		}

		anchorVC, err := verifiable.ParseCredential([]byte(anchorCred),
			verifiable.WithDisabledProofCheck(),
			verifiable.WithJSONLDDocumentLoader(testutil.GetLoader(t)),
		)
		require.NoError(t, err)

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		err = c.postOfferActivity(anchorVC, []string{"https://abc.com/services/orb"})
		require.NoError(t, err)
	})

	t.Run("error - get witnesses URIs error", func(t *testing.T) {
		providers := &Providers{
			Outbox: &mockOutbox{},
		}

		anchorVC, err := verifiable.ParseCredential([]byte(anchorCred),
			verifiable.WithDisabledProofCheck(),
			verifiable.WithJSONLDDocumentLoader(testutil.GetLoader(t)),
		)
		require.NoError(t, err)

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		err = c.postOfferActivity(anchorVC, []string{":xyz"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing protocol scheme")
	})

	t.Run("error - get system URI error", func(t *testing.T) {
		providers := &Providers{
			Outbox:        &mockOutbox{},
			ActivityStore: &mockActivityStore{},
			WitnessStore:  &mockWitnessStore{},
		}

		anchorVC, err := verifiable.ParseCredential([]byte(anchorCred),
			verifiable.WithDisabledProofCheck(),
			verifiable.WithJSONLDDocumentLoader(testutil.GetLoader(t)),
		)
		require.NoError(t, err)

		c, err := New(namespace, &url.URL{Host: "?!?"}, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		err = c.postOfferActivity(anchorVC, []string{"https://abc.com/services/orb"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to parse system witness path")
	})

	t.Run("error - witness proof store error", func(t *testing.T) {
		providers := &Providers{
			Outbox:        &mockOutbox{},
			WitnessStore:  &mockWitnessStore{PutErr: fmt.Errorf("witness store error")},
			ActivityStore: &mockActivityStore{},
			WFClient:      wfClient,
		}

		anchorVC, err := verifiable.ParseCredential([]byte(anchorCred),
			verifiable.WithDisabledProofCheck(),
			verifiable.WithJSONLDDocumentLoader(testutil.GetLoader(t)),
		)
		require.NoError(t, err)

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		err = c.postOfferActivity(anchorVC, []string{"https://abc.com/services/orb"})
		require.Error(t, err)
		require.Contains(t, err.Error(),
			"failed to store witnesses for vcID[http://peer1.com/vc/62c153d1-a6be-400e-a6a6-5b700b596d9d]: witness store error")
	})

	t.Run("error - webfinger client error (batch and system witness)", func(t *testing.T) {
		wfClientWithErr := wfclient.New(wfclient.WithHTTPClient(
			httpMock(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					Body:       ioutil.NopCloser(bytes.NewBufferString("internal server error")),
					StatusCode: http.StatusInternalServerError,
				}, nil
			})))

		providers := &Providers{
			Outbox:        &mockOutbox{},
			WitnessStore:  &mockWitnessStore{},
			ActivityStore: &mockActivityStore{},
			WFClient:      wfClientWithErr,
		}

		anchorVC, err := verifiable.ParseCredential([]byte(anchorCred),
			verifiable.WithDisabledProofCheck(),
			verifiable.WithJSONLDDocumentLoader(testutil.GetLoader(t)),
		)
		require.NoError(t, err)

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		// test error for batch witness
		err = c.postOfferActivity(anchorVC, []string{"https://abc.com/services/orb"})
		require.Error(t, err)
		require.Contains(t, err.Error(),
			"failed to resolve WebFinger resource: received unexpected status code. URL [https://abc.com/.well-known/webfinger?resource=https://abc.com/vct], status code [500], response body [internal server error]") //nolint:lll

		// test error for system witness (no batch witnesses)
		err = c.postOfferActivity(anchorVC, []string{})
		require.Error(t, err)
		require.Contains(t, err.Error(),
			"failed to resolve WebFinger resource: received unexpected status code. URL [http://orb.domain1.com/.well-known/webfinger?resource=http://orb.domain1.com/vct], status code [500], response body [internal server error]") //nolint:lll
	})

	t.Run("error - activity store error", func(t *testing.T) {
		providers := &Providers{
			Outbox:        &mockOutbox{},
			WitnessStore:  &mockWitnessStore{},
			ActivityStore: &mockActivityStore{Err: fmt.Errorf("activity store error")},
			WFClient:      wfClient,
		}

		anchorVC, err := verifiable.ParseCredential([]byte(anchorCred),
			verifiable.WithDisabledProofCheck(),
			verifiable.WithJSONLDDocumentLoader(testutil.GetLoader(t)),
		)
		require.NoError(t, err)

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		err = c.postOfferActivity(anchorVC, []string{"https://abc.com/services/orb"})
		require.Error(t, err)
		require.Contains(t, err.Error(),
			"failed to query references for system witnesses: activity store error")
	})

	t.Run("error - post offer to outbox error", func(t *testing.T) {
		providers := &Providers{
			Outbox:        &mockOutbox{Err: fmt.Errorf("outbox error")},
			WitnessStore:  &mockWitnessStore{},
			ActivityStore: &mockActivityStore{},
			VCStatusStore: &mockVCStatusStore{},
			WFClient:      wfClient,
		}

		anchorVC, err := verifiable.ParseCredential([]byte(anchorCred),
			verifiable.WithDisabledProofCheck(),
			verifiable.WithJSONLDDocumentLoader(testutil.GetLoader(t)),
		)
		require.NoError(t, err)

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		err = c.postOfferActivity(anchorVC, []string{"https://abc.com/services/orb"})
		require.Error(t, err)
		require.Contains(t, err.Error(),
			"failed to post offer for vcID[http://peer1.com/vc/62c153d1-a6be-400e-a6a6-5b700b596d9d]: outbox error")
	})
}

func TestWriter_getWitnesses(t *testing.T) {
	ps := mempubsub.New(mempubsub.Config{})
	defer ps.Stop()

	apServiceIRI, err := url.Parse(activityPubURL)
	require.NoError(t, err)

	casIRI, err := url.Parse(casURL)
	require.NoError(t, err)

	t.Run("success", func(t *testing.T) {
		var numTimesMockServerHandlerHit int

		var testServerURL string

		testServer := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if numTimesMockServerHandlerHit == 5 {
					// Ensure that one of the witnesses returned is a duplicate of another one.
					_, err = w.Write(generateValidExampleHostMetaResponse(t, fmt.Sprintf("%s/4", testServerURL)))
					require.NoError(t, err)
				} else {
					_, err = w.Write(generateValidExampleHostMetaResponse(t, fmt.Sprintf("%s/%d", testServerURL,
						numTimesMockServerHandlerHit)))
					require.NoError(t, err)
				}

				numTimesMockServerHandlerHit++
			}))
		defer testServer.Close()

		testServerURL = testServer.URL

		testAnchorOrigin := fmt.Sprintf("%s/services/orb", testServerURL)

		opMap := map[string]*protocol.ResolutionModel{
			"did-1": {AnchorOrigin: testAnchorOrigin},
			"did-2": {AnchorOrigin: testAnchorOrigin},
			"did-3": {AnchorOrigin: testAnchorOrigin},
		}

		providers := &Providers{
			OpProcessor: &mockOpProcessor{Map: opMap},
		}

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t),
			resourceresolver.New(http.DefaultClient, nil, resourceresolver.WithCacheLifetime(0)),
			&mocks.MetricsProvider{})
		require.NoError(t, err)

		opRefs := []*operation.Reference{
			{
				UniqueSuffix: "did-1",
				Type:         operation.TypeUpdate,
			},
			{
				UniqueSuffix: "did-2",
				Type:         operation.TypeRecover,
				AnchorOrigin: testAnchorOrigin,
			},
			{
				UniqueSuffix: "did-3",
				Type:         operation.TypeDeactivate,
			},
			{
				UniqueSuffix: "did-4",
				Type:         operation.TypeCreate,
				AnchorOrigin: testAnchorOrigin,
			},
			{
				UniqueSuffix: "did-5",
				Type:         operation.TypeCreate,
				AnchorOrigin: testAnchorOrigin,
			},
			{
				UniqueSuffix: "did-6",
				Type:         operation.TypeCreate,
				AnchorOrigin: testAnchorOrigin, // test re-use same origin (this will be the same as the one above)
			},
		}

		witnesses, err := c.getWitnesses(opRefs)
		require.NoError(t, err)
		require.Equal(t, 5, len(witnesses))

		expectedWitnessTemplate := "%s/%d/services/orb"

		expected := []string{
			fmt.Sprintf(expectedWitnessTemplate, testServerURL, 0),
			fmt.Sprintf(expectedWitnessTemplate, testServerURL, 1),
			fmt.Sprintf(expectedWitnessTemplate, testServerURL, 2),
			fmt.Sprintf(expectedWitnessTemplate, testServerURL, 3),
			fmt.Sprintf(expectedWitnessTemplate, testServerURL, 4),
		}
		require.Equal(t, expected, witnesses)
	})

	t.Run("error - operation type not supported", func(t *testing.T) {
		opMap := map[string]*protocol.ResolutionModel{
			"did-1": {AnchorOrigin: "origin-1.com"},
		}

		providers := &Providers{
			OpProcessor: &mockOpProcessor{Map: opMap},
		}

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		opRefs := []*operation.Reference{
			{
				UniqueSuffix: "did-1",
				Type:         "invalid",
			},
		}

		witnesses, err := c.getWitnesses(opRefs)
		require.Error(t, err)
		require.Nil(t, witnesses)
		require.Equal(t, err.Error(), "operation type 'invalid' not supported for assembling witness list")
	})

	t.Run("error - unexpected interface for anchor origin", func(t *testing.T) {
		opMap := map[string]*protocol.ResolutionModel{
			"did-1": {AnchorOrigin: 0},
		}

		providers := &Providers{
			OpProcessor: &mockOpProcessor{Map: opMap},
		}

		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		opRefs := []*operation.Reference{
			{
				UniqueSuffix: "did-1",
				Type:         operation.TypeUpdate,
			},
		}

		witnesses, err := c.getWitnesses(opRefs)
		require.Error(t, err)
		require.Nil(t, witnesses)
		require.Contains(t, err.Error(), "unexpected interface 'int' for anchor origin")
	})
}

func TestWriter_getBatchWitnessesIRI(t *testing.T) {
	ps := mempubsub.New(mempubsub.Config{})
	defer ps.Stop()

	apServiceIRI, err := url.Parse(activityPubURL)
	require.NoError(t, err)

	c, err := New(namespace, apServiceIRI, nil, &Providers{}, &anchormocks.AnchorPublisher{}, ps,
		testMaxWitnessDelay, true, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
	require.NoError(t, err)

	t.Run("success", func(t *testing.T) {
		witnesses := []string{"origin-1.com", "origin-2.com"}

		witnessesIRI, err := c.getBatchWitnessesIRI(witnesses)
		require.NoError(t, err)

		// two from witness list
		require.Equal(t, 2, len(witnessesIRI))
	})

	t.Run("success - exclude current domain from witness list", func(t *testing.T) {
		witnesses := []string{activityPubURL}

		witnessesIRI, err := c.getBatchWitnessesIRI(witnesses)
		require.NoError(t, err)

		require.Equal(t, 0, len(witnessesIRI))
	})

	t.Run("error - invalid url", func(t *testing.T) {
		witnesses := []string{":xyz"}

		witnessesIRI, err := c.getBatchWitnessesIRI(witnesses)
		require.Error(t, err)
		require.Nil(t, witnessesIRI)
		require.Contains(t, err.Error(), "missing protocol scheme")
	})
}

func TestWriter_Read(t *testing.T) {
	ps := mempubsub.New(mempubsub.Config{})
	defer ps.Stop()

	casClient, err := cas.New(mem.NewProvider(), casURL, nil, &mocks.MetricsProvider{}, 0)

	require.NoError(t, err)

	graphProviders := &graph.Providers{
		CasWriter: casClient,
		CasResolver: casresolver.New(casClient, nil,
			casresolver.NewWebCASResolver(
				transport.New(&http.Client{}, testutil.MustParseURL("https://example.com/keys/public-key"),
					transport.DefaultSigner(), transport.DefaultSigner()),
				wfclient.New(), "https"), &mocks.MetricsProvider{}),
		Pkf: pubKeyFetcherFnc,
	}

	providers := &Providers{
		AnchorGraph: graph.New(graphProviders),
		DidAnchors:  memdidanchor.New(),
	}

	apServiceIRI, err := url.Parse(activityPubURL)
	require.NoError(t, err)

	casIRI, err := url.Parse(casURL)
	require.NoError(t, err)

	t.Run("success", func(t *testing.T) {
		c, err := New(namespace, apServiceIRI, casIRI, providers, &anchormocks.AnchorPublisher{}, ps,
			testMaxWitnessDelay, signWithLocalWitness, testutil.GetLoader(t), nil, &mocks.MetricsProvider{})
		require.NoError(t, err)

		more, entries := c.Read(-1)
		require.False(t, more)
		require.Empty(t, entries)
	})
}

type mockTxnBuilder struct {
	Err error
}

func (m *mockTxnBuilder) Build(payload *subject.Payload) (*verifiable.Credential, error) {
	if m.Err != nil {
		return nil, m.Err
	}

	return &verifiable.Credential{Subject: payload, ID: "http://domain.com/vc/123"}, nil
}

var pubKeyFetcherFnc = func(issuerID, keyID string) (*verifier.PublicKey, error) {
	return nil, nil
}

type mockAnchorGraph struct {
	Err error
}

func (m *mockAnchorGraph) Add(vc *verifiable.Credential) (string, error) {
	if m.Err != nil {
		return "", m.Err
	}

	return "cid", nil
}

type mockDidAnchor struct {
	Err error
}

func (m *mockDidAnchor) GetBulk(did []string) ([]string, error) {
	return []string{"cid"}, nil
}

type mockOpProcessor struct {
	Err error
	Map map[string]*protocol.ResolutionModel
}

func (m *mockOpProcessor) Resolve(uniqueSuffix string) (*protocol.ResolutionModel, error) {
	if m.Err != nil {
		return nil, m.Err
	}

	return m.Map[uniqueSuffix], nil
}

type mockOutbox struct {
	Err error
}

func (m *mockOutbox) Post(activity *vocab.ActivityType) (*url.URL, error) {
	if m.Err != nil {
		return nil, m.Err
	}

	return activity.ID().URL(), nil
}

type mockSigner struct {
	Err error
}

func (m *mockSigner) Sign(vc *verifiable.Credential, opts ...vcsigner.Opt) (*verifiable.Credential, error) {
	if m.Err != nil {
		return nil, m.Err
	}

	return vc, nil
}

type mockWitness struct {
	proofBytes []byte
	Err        error
}

func (w *mockWitness) Witness(anchorCred []byte) ([]byte, error) {
	if w.Err != nil {
		return nil, w.Err
	}

	if len(w.proofBytes) != 0 {
		return w.proofBytes, nil
	}

	return anchorCred, nil
}

type mockMonitoring struct {
	Err error
}

func (m *mockMonitoring) Watch(_ *verifiable.Credential, _ time.Time, _ string, _ time.Time) error {
	if m.Err != nil {
		return m.Err
	}

	return nil
}

type mockActivityStore struct {
	Err error
}

func (a *mockActivityStore) QueryReferences(refType spi.ReferenceType, query *spi.Criteria, opts ...spi.QueryOpt) (spi.ReferenceIterator, error) { // nolint: lll
	if a.Err != nil {
		return nil, a.Err
	}

	systemWitnessIRI, err := url.Parse("http://orb.domain1.com/services/orb")
	if err != nil {
		return nil, err
	}

	iter := &apmocks.ReferenceIterator{}

	iter.NextReturnsOnCall(0, systemWitnessIRI, nil)
	iter.NextReturnsOnCall(1, nil, spi.ErrNotFound)

	return iter, nil
}

type mockWitnessStore struct {
	PutErr    error
	DeleteErr error
}

func (w *mockWitnessStore) Put(vcID string, witnesses []*proof.WitnessProof) error {
	if w.PutErr != nil {
		return w.PutErr
	}

	return nil
}

func (w *mockWitnessStore) Delete(vcID string) error {
	if w.DeleteErr != nil {
		return w.DeleteErr
	}

	return nil
}

type mockVCStatusStore struct {
	Err error
}

func (ss *mockVCStatusStore) AddStatus(vcID string, status proof.VCStatus) error {
	if ss.Err != nil {
		return ss.Err
	}

	return nil
}

func getOperationReferences(anchorOrigin string) []*operation.Reference {
	return []*operation.Reference{
		{
			UniqueSuffix: "did-1",
			Type:         operation.TypeCreate,
			AnchorOrigin: anchorOrigin,
		},
	}
}

func generateValidExampleHostMetaResponse(t *testing.T, hostnameInResponse string) []byte {
	t.Helper()

	hostMetaResponse := discoveryrest.JRD{
		Subject:    "",
		Properties: nil,
		Links: []discoveryrest.Link{
			{
				Type: discoveryrest.ActivityJSONType,
				Href: fmt.Sprintf("%s/services/orb", hostnameInResponse),
			},
		},
	}

	hostMetaResponseBytes, err := json.Marshal(hostMetaResponse)
	require.NoError(t, err)

	return hostMetaResponseBytes
}

type httpMock func(req *http.Request) (*http.Response, error)

func (m httpMock) Do(req *http.Request) (*http.Response, error) {
	return m(req)
}

//nolint: lll
var anchorCred = `
{
  "@context": [
    "https://www.w3.org/2018/credentials/v1"
  ],
  "credentialSubject": {
    "coreIndex": "QmZzPwGc3JEMQDiJu21YZcdpEPam7qCoXPLEUQXn34sMhB",
    "namespace": "did:sidetree",
    "operationCount": 1,
    "previousAnchors": {
      "EiBjG9z921eyj8wI4j-LAqsJBRC_GalIUWPJeXGekxFQ-w": ""
    },
    "version": 0
  },
  "id": "http://peer1.com/vc/62c153d1-a6be-400e-a6a6-5b700b596d9d",
  "issuanceDate": "2021-03-17T20:01:10.4002903Z",
  "issuer": "http://peer1.com",
  "proof": {
    "created": "2021-03-17T20:01:10.4024292Z",
    "domain": "domain.com",
    "jws": "eyJhbGciOiJFZERTQSIsImI2NCI6ZmFsc2UsImNyaXQiOlsiYjY0Il19..pHA1rMSsHBJLbDwRpNY0FrgSgoLzBw4S7VP7d5bkYW-JwU8qc_4CmPfQctR8kycQHSa2Jh8LNBqNKMeVWsAwDA",
    "proofPurpose": "assertionMethod",
    "type": "Ed25519Signature2018",
    "verificationMethod": "did:web:abc#CvSyX0VxMCbg-UiYpAVd9OmhaFBXBr5ISpv2RZ2c9DY"
  },
  "type": "VerifiableCredential"
}`
