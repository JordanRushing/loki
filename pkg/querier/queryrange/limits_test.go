package queryrange

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/grafana/dskit/user"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
	"gopkg.in/yaml.v2"

	"github.com/grafana/loki/v3/pkg/logproto"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/grafana/loki/v3/pkg/logqlmodel"
	"github.com/grafana/loki/v3/pkg/logqlmodel/metadata"
	"github.com/grafana/loki/v3/pkg/querier/plan"
	"github.com/grafana/loki/v3/pkg/querier/queryrange/queryrangebase"
	"github.com/grafana/loki/v3/pkg/storage/config"
	"github.com/grafana/loki/v3/pkg/storage/types"
	"github.com/grafana/loki/v3/pkg/util"
	"github.com/grafana/loki/v3/pkg/util/constants"
	"github.com/grafana/loki/v3/pkg/util/httpreq"
	util_log "github.com/grafana/loki/v3/pkg/util/log"
)

func TestLimits(t *testing.T) {
	l := fakeLimits{
		splitDuration: map[string]time.Duration{"a": time.Minute},
	}

	wrapped := WithSplitByLimits(l, time.Hour)

	// Test default
	require.Equal(t, wrapped.QuerySplitDuration("b"), time.Hour)
	// Ensure we override the underlying implementation
	require.Equal(t, wrapped.QuerySplitDuration("a"), time.Hour)

	r := &LokiRequest{
		Query:   "qry",
		StartTs: time.Now(),
		Step:    int64(time.Minute / time.Millisecond),
	}

	require.Equal(
		t,
		fmt.Sprintf("%s:%s:%d:%d:%d", "a", r.GetQuery(), r.GetStep(), r.GetStart().UnixMilli()/int64(time.Hour/time.Millisecond), int64(time.Hour)),
		cacheKeyLimits{wrapped, nil, nil}.GenerateCacheKey(context.Background(), "a", r),
	)
}

func TestMetricQueryCacheKey(t *testing.T) {
	const (
		defaultTenant       = "a"
		alternateTenant     = "b"
		query               = `sum(rate({foo="bar"}[1]))`
		defaultSplit        = time.Hour
		ingesterSplit       = 90 * time.Minute
		ingesterQueryWindow = defaultSplit * 3
	)

	step := (15 * time.Second).Milliseconds()

	l := fakeLimits{
		splitDuration:         map[string]time.Duration{defaultTenant: defaultSplit, alternateTenant: defaultSplit},
		ingesterSplitDuration: map[string]time.Duration{defaultTenant: ingesterSplit},
	}

	cases := []struct {
		name, tenantID string
		start, end     time.Time
		expectedSplit  time.Duration
		iqo            util.IngesterQueryOptions
	}{
		{
			name:          "outside ingester query window",
			tenantID:      defaultTenant,
			start:         time.Now().Add(-6 * time.Hour),
			end:           time.Now().Add(-5 * time.Hour),
			expectedSplit: defaultSplit,
			iqo: ingesterQueryOpts{
				queryIngestersWithin: ingesterQueryWindow,
				queryStoreOnly:       false,
			},
		},
		{
			name:          "within ingester query window",
			tenantID:      defaultTenant,
			start:         time.Now().Add(-6 * time.Hour),
			end:           time.Now().Add(-ingesterQueryWindow / 2),
			expectedSplit: ingesterSplit,
			iqo: ingesterQueryOpts{
				queryIngestersWithin: ingesterQueryWindow,
				queryStoreOnly:       false,
			},
		},
		{
			name:          "within ingester query window, but query store only",
			tenantID:      defaultTenant,
			start:         time.Now().Add(-6 * time.Hour),
			end:           time.Now().Add(-ingesterQueryWindow / 2),
			expectedSplit: defaultSplit,
			iqo: ingesterQueryOpts{
				queryIngestersWithin: ingesterQueryWindow,
				queryStoreOnly:       true,
			},
		},
		{
			name:          "within ingester query window, but no ingester split duration configured",
			tenantID:      alternateTenant,
			start:         time.Now().Add(-6 * time.Hour),
			end:           time.Now().Add(-ingesterQueryWindow / 2),
			expectedSplit: defaultSplit,
			iqo: ingesterQueryOpts{
				queryIngestersWithin: ingesterQueryWindow,
				queryStoreOnly:       false,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keyGen := cacheKeyLimits{l, nil, tc.iqo}

			r := &LokiRequest{
				Query:   query,
				StartTs: tc.start,
				EndTs:   tc.end,
				Step:    step,
			}

			// we use regex here because cache key always refers to the current time to get the ingester query window,
			// and therefore we can't know the current interval apriori without duplicating the logic
			pattern := regexp.MustCompile(fmt.Sprintf(`%s:%s:%d:(\d+):%d`, tc.tenantID, regexp.QuoteMeta(query), step, tc.expectedSplit))
			require.Regexp(t, pattern, keyGen.GenerateCacheKey(context.Background(), tc.tenantID, r))
		})
	}
}

func Test_seriesLimiter(t *testing.T) {
	cfg := testConfig
	cfg.CacheResults = false
	cfg.CacheIndexStatsResults = false
	// split in 7 with 2 in // max.
	l := WithSplitByLimits(fakeLimits{maxSeries: 1, maxQueryParallelism: 2}, time.Hour)
	tpw, stopper, err := NewMiddleware(cfg, testEngineOpts, nil, util_log.Logger, l, config.SchemaConfig{
		Configs: testSchemas,
	}, nil, false, nil, constants.Loki)
	if stopper != nil {
		defer stopper.Stop()
	}
	require.NoError(t, err)

	lreq := &LokiRequest{
		Query:     `rate({app="foo"} |= "foo"[1m])`,
		Limit:     1000,
		Step:      30000, // 30sec
		StartTs:   testTime.Add(-6 * time.Hour),
		EndTs:     testTime,
		Direction: logproto.FORWARD,
		Path:      "/query_range",
		Plan: &plan.QueryPlan{
			AST: syntax.MustParseExpr(`rate({app="foo"} |= "foo"[1m])`),
		},
	}

	ctx := user.InjectOrgID(context.Background(), "1")

	count, h := promqlResult(matrix)
	_, err = tpw.Wrap(h).Do(ctx, lreq)
	require.NoError(t, err)
	require.Equal(t, 7, *count)

	// 2 series should not be allowed.
	c := new(int)
	m := &sync.Mutex{}
	h = queryrangebase.HandlerFunc(func(_ context.Context, req queryrangebase.Request) (queryrangebase.Response, error) {
		m.Lock()
		defer m.Unlock()
		defer func() {
			*c++
		}()
		// first time returns  a single series
		if *c == 0 {
			params, err := ParamsFromRequest(req)
			if err != nil {
				return nil, err
			}
			return ResultToResponse(logqlmodel.Result{Data: matrix}, params)
		}
		// second time returns a different series.
		m := promql.Matrix{
			{
				Floats: []promql.FPoint{
					{
						T: toMs(testTime.Add(-4 * time.Hour)),
						F: 0.013333333333333334,
					},
				},
				Metric: labels.FromStrings(
					"filename", `/var/hostlog/apport.log`,
					"job", "anotherjob",
				),
			},
		}
		params, err := ParamsFromRequest(req)
		if err != nil {
			return nil, err
		}
		return ResultToResponse(logqlmodel.Result{Data: m}, params)
	})

	_, err = tpw.Wrap(h).Do(ctx, lreq)
	require.Error(t, err)
	require.LessOrEqual(t, *c, 4)
}

func Test_seriesLimiterDrilldown(t *testing.T) {
	// Test the drilldown scenario directly using the series limiter middleware
	middleware := newSeriesLimiter(1) // limit to 1 series

	// Create context with drilldown request headers
	tenantCtx := user.InjectOrgID(context.Background(), "1")
	ctx := httpreq.InjectQueryTags(tenantCtx, "Source="+constants.LogsDrilldownAppName)

	// Create metadata context to capture warnings
	md, ctx := metadata.NewContext(ctx)

	callCount := 0
	// Create a handler that returns 1 series on first call, then 1 more series on second call
	h := queryrangebase.HandlerFunc(func(_ context.Context, _ queryrangebase.Request) (queryrangebase.Response, error) {
		var result []queryrangebase.SampleStream

		if callCount == 0 {
			// First call: return 1 series
			result = []queryrangebase.SampleStream{
				{
					Labels: []logproto.LabelAdapter{
						{Name: "filename", Value: "/var/hostlog/app.log"},
						{Name: "job", Value: "firstjob"},
					},
					Samples: []logproto.LegacySample{{Value: 0.013333333333333334}},
				},
			}
		} else {
			// Second call: return 1 different series (this should trigger the drilldown logic)
			result = []queryrangebase.SampleStream{
				{
					Labels: []logproto.LabelAdapter{
						{Name: "filename", Value: "/var/hostlog/apport.log"},
						{Name: "job", Value: "anotherjob"},
					},
					Samples: []logproto.LegacySample{{Value: 0.026666666666666668}},
				},
			}
		}
		callCount++

		return &LokiPromResponse{
			Response: &queryrangebase.PrometheusResponse{
				Data: queryrangebase.PrometheusData{
					ResultType: "matrix",
					Result:     result,
				},
			},
		}, nil
	})

	wrappedHandler := middleware.Wrap(h)

	// First call - should succeed and add 1 series to the limiter
	resp1, err := wrappedHandler.Do(ctx, &LokiRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp1)

	// Should have no warnings yet
	require.Len(t, md.Warnings(), 0)

	// Second call - should trigger drilldown logic and return with warning
	resp2, err := wrappedHandler.Do(ctx, &LokiRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp2)

	// Verify that a warning was added about partial results
	warnings := md.Warnings()
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], "maximum number of series (1) reached for a single query; returning partial results")

	// The second response should be the original response since drilldown returns early
	lokiResp, ok := resp2.(*LokiPromResponse)
	require.True(t, ok)
	require.Len(t, lokiResp.Response.Data.Result, 1)

	// A request without the drilldown header should produce an error
	_, err = wrappedHandler.Do(tenantCtx, &LokiRequest{})
	require.Error(t, err)
}

func TestSeriesLimiter_PerVariantLimits(t *testing.T) {
	for _, test := range []struct {
		name             string
		req              *LokiRequest
		resp             *LokiPromResponse
		expectedResponse *LokiPromResponse
		expectedWarnings []string
	}{
		{
			name: "single variant under limit should not exceed limit",
			req: &LokiRequest{
				Query: "sum by (job) (count_over_time({job=~\"app.*\"}[1m]))",
			},
			resp: createPromResponse([][]seriesLabels{
				{
					{Name: "job", Value: "app1"},
					{Name: constants.VariantLabel, Value: "0"},
				},
				{
					{Name: "job", Value: "app2"},
					{Name: constants.VariantLabel, Value: "0"},
				},
				{
					{Name: "job", Value: "app3"},
					{Name: constants.VariantLabel, Value: "0"},
				},
			}),
			expectedResponse: createPromResponse([][]seriesLabels{
				{
					{Name: "job", Value: "app1"},
					{Name: constants.VariantLabel, Value: "0"},
				},
				{
					{Name: "job", Value: "app2"},
					{Name: constants.VariantLabel, Value: "0"},
				},
				{
					{Name: "job", Value: "app3"},
					{Name: constants.VariantLabel, Value: "0"},
				},
			}),
		},
		{
			name: "multiple variants under limit should not exceed limit",
			req: &LokiRequest{
				Query: "sum by (job) (count_over_time({job=~\"app.*\"}[1m]))",
			},
			resp: createPromResponse([][]seriesLabels{
				{
					{Name: "job", Value: "app1"},
					{Name: constants.VariantLabel, Value: "0"},
				},
				{
					{Name: "job", Value: "app2"},
					{Name: constants.VariantLabel, Value: "0"},
				},
				{
					{Name: "job", Value: "app3"},
					{Name: constants.VariantLabel, Value: "0"},
				},
				{
					{Name: "job", Value: "app1"},
					{Name: constants.VariantLabel, Value: "1"},
				},
				{
					{Name: "job", Value: "app2"},
					{Name: constants.VariantLabel, Value: "1"},
				},
				{
					{Name: "job", Value: "app3"},
					{Name: constants.VariantLabel, Value: "1"},
				},
			}),
			expectedResponse: createPromResponse([][]seriesLabels{
				{
					{Name: "job", Value: "app1"},
					{Name: constants.VariantLabel, Value: "0"},
				},
				{
					{Name: "job", Value: "app2"},
					{Name: constants.VariantLabel, Value: "0"},
				},
				{
					{Name: "job", Value: "app3"},
					{Name: constants.VariantLabel, Value: "0"},
				},
				{
					{Name: "job", Value: "app1"},
					{Name: constants.VariantLabel, Value: "1"},
				},
				{
					{Name: "job", Value: "app2"},
					{Name: constants.VariantLabel, Value: "1"},
				},
				{
					{Name: "job", Value: "app3"},
					{Name: constants.VariantLabel, Value: "1"},
				},
			}),
		},
		{
			name: "single variant over the limit should exceed limit",
			req: &LokiRequest{
				Query: "sum by (job) (count_over_time({job=~\"app.*\"}[1m]))",
			},
			resp: createPromResponse([][]seriesLabels{
				{
					{Name: "job", Value: "app1"},
					{Name: constants.VariantLabel, Value: "1"},
				},
				{
					{Name: "job", Value: "app2"},
					{Name: constants.VariantLabel, Value: "1"},
				},
				{
					{Name: "job", Value: "app3"},
					{Name: constants.VariantLabel, Value: "1"},
				},
				{
					{Name: "job", Value: "app4"},
					{Name: constants.VariantLabel, Value: "1"},
				},
			}),
			expectedResponse: createPromResponse([][]seriesLabels{
				{
					{Name: "job", Value: "app1"},
					{Name: constants.VariantLabel, Value: "1"},
				},
				{
					{Name: "job", Value: "app2"},
					{Name: constants.VariantLabel, Value: "1"},
				},
				{
					{Name: "job", Value: "app3"},
					{Name: constants.VariantLabel, Value: "1"},
				},
			}),
			expectedWarnings: []string{"maximum of series (3) reached for variant (1)"},
		},
		{
			name: "multiple variants over the limit should exceed limit",
			req: &LokiRequest{
				Query: "sum by (job) (count_over_time({job=~\"app.*\"}[1m]))",
			},
			resp: createPromResponse([][]seriesLabels{
				{
					{Name: "app", Value: "foo"},
					{Name: constants.VariantLabel, Value: "0"},
				},
				{
					{Name: "app", Value: "bar"},
					{Name: constants.VariantLabel, Value: "0"},
				},
				{
					{Name: "job", Value: "app1"},
					{Name: constants.VariantLabel, Value: "1"},
				},
				{
					{Name: "job", Value: "app2"},
					{Name: constants.VariantLabel, Value: "1"},
				},
				{
					{Name: "job", Value: "app3"},
					{Name: constants.VariantLabel, Value: "1"},
				},
				{
					{Name: "job", Value: "app4"},
					{Name: constants.VariantLabel, Value: "1"},
				},
				{
					{Name: "job", Value: "app5"},
					{Name: constants.VariantLabel, Value: "1"},
				},
			}),
			expectedResponse: createPromResponse([][]seriesLabels{
				{
					{Name: "app", Value: "foo"},
					{Name: constants.VariantLabel, Value: "0"},
				},
				{
					{Name: "app", Value: "bar"},
					{Name: constants.VariantLabel, Value: "0"},
				},
				{
					{Name: "job", Value: "app1"},
					{Name: constants.VariantLabel, Value: "1"},
				},
				{
					{Name: "job", Value: "app2"},
					{Name: constants.VariantLabel, Value: "1"},
				},
				{
					{Name: "job", Value: "app3"},
					{Name: constants.VariantLabel, Value: "1"},
				},
			}),
			expectedWarnings: []string{"maximum of series (3) reached for variant (1)"},
		},
	} {

		t.Run(test.name, func(t *testing.T) {
			middleware := newSeriesLimiter(3)
			mock := variantMockHandler{
				response: test.resp,
			}
			handler := middleware.Wrap(mock)

			metadata, ctx := metadata.NewContext(context.Background())

			resp, err := handler.Do(ctx, &LokiRequest{})
			require.NoError(t, err)
			require.EqualValues(t, test.expectedResponse.Response.Data, resp.(*LokiPromResponse).Response.Data)

			if test.expectedWarnings != nil {
				require.Equal(t, test.expectedWarnings, metadata.Warnings())
			}
		})
	}
}

type seriesLabels = logproto.LabelAdapter

// variantMockHandler is a mock implementation of queryrangebase.Handler
type variantMockHandler struct {
	response *LokiPromResponse
}

func (m variantMockHandler) Do(_ context.Context, _ queryrangebase.Request) (queryrangebase.Response, error) {
	// For testing, we'll return the predefined responses
	// This is just a mock - in a real case, the handler would return
	// different responses based on the request
	if m.response != nil {
		return m.response, nil
	}
	// Return empty response by default
	return createPromResponse(nil), nil
}

func createPromResponse(series [][]seriesLabels) *LokiPromResponse {
	result := make([]queryrangebase.SampleStream, len(series))
	for i, labels := range series {
		result[i] = queryrangebase.SampleStream{
			Labels:  labels,
			Samples: []logproto.LegacySample{{Value: 1.0}},
		}
	}

	return &LokiPromResponse{
		Response: &queryrangebase.PrometheusResponse{
			Data: queryrangebase.PrometheusData{
				ResultType: "matrix",
				Result:     result,
			},
		},
	}
}

func Test_MaxQueryParallelism(t *testing.T) {
	maxQueryParallelism := 2

	var count atomic.Int32
	var maxVal atomic.Int32
	h := queryrangebase.HandlerFunc(func(_ context.Context, _ queryrangebase.Request) (queryrangebase.Response, error) {
		cur := count.Inc()
		if cur > maxVal.Load() {
			maxVal.Store(cur)
		}
		defer count.Dec()
		// simulate some work
		time.Sleep(20 * time.Millisecond)
		return queryrangebase.NewEmptyPrometheusResponse(model.ValMatrix), nil
	})
	ctx := user.InjectOrgID(context.Background(), "foo")

	_, _ = NewLimitedRoundTripper(h, fakeLimits{maxQueryParallelism: maxQueryParallelism},
		testSchemas,
		queryrangebase.MiddlewareFunc(func(next queryrangebase.Handler) queryrangebase.Handler {
			return queryrangebase.HandlerFunc(func(c context.Context, _ queryrangebase.Request) (queryrangebase.Response, error) {
				var wg sync.WaitGroup
				for i := 0; i < 10; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						_, _ = next.Do(c, &LokiRequest{})
					}()
				}
				wg.Wait()
				return nil, nil
			})
		}),
	).Do(ctx, &LokiRequest{})
	maxFound := int(maxVal.Load())
	require.LessOrEqual(t, maxFound, maxQueryParallelism, "max query parallelism: ", maxFound, " went over the configured one:", maxQueryParallelism)
}

func Test_MaxQueryParallelismLateScheduling(t *testing.T) {
	maxQueryParallelism := 2

	h := queryrangebase.HandlerFunc(func(_ context.Context, _ queryrangebase.Request) (queryrangebase.Response, error) {
		// simulate some work
		time.Sleep(20 * time.Millisecond)
		return queryrangebase.NewEmptyPrometheusResponse(model.ValMatrix), nil
	})
	ctx := user.InjectOrgID(context.Background(), "foo")

	_, err := NewLimitedRoundTripper(h, fakeLimits{maxQueryParallelism: maxQueryParallelism},
		testSchemas,
		queryrangebase.MiddlewareFunc(func(next queryrangebase.Handler) queryrangebase.Handler {
			return queryrangebase.HandlerFunc(func(c context.Context, r queryrangebase.Request) (queryrangebase.Response, error) {
				for i := 0; i < 10; i++ {
					go func() {
						_, _ = next.Do(c, r)
					}()
				}
				return nil, nil
			})
		}),
	).Do(ctx, &LokiRequest{})

	require.NoError(t, err)
}

func Test_MaxQueryParallelismDisable(t *testing.T) {
	maxQueryParallelism := 0

	h := queryrangebase.HandlerFunc(func(_ context.Context, _ queryrangebase.Request) (queryrangebase.Response, error) {
		// simulate some work
		time.Sleep(20 * time.Millisecond)
		return queryrangebase.NewEmptyPrometheusResponse(model.ValMatrix), nil
	})
	ctx := user.InjectOrgID(context.Background(), "foo")

	_, err := NewLimitedRoundTripper(h, fakeLimits{maxQueryParallelism: maxQueryParallelism},
		testSchemas,
		queryrangebase.MiddlewareFunc(func(next queryrangebase.Handler) queryrangebase.Handler {
			return queryrangebase.HandlerFunc(func(c context.Context, _ queryrangebase.Request) (queryrangebase.Response, error) {
				for i := 0; i < 10; i++ {
					go func() {
						_, _ = next.Do(c, &LokiRequest{})
					}()
				}
				return nil, nil
			})
		}),
	).Do(ctx, &LokiRequest{})
	require.Error(t, err)
}

func Test_MaxQueryLookBack(t *testing.T) {
	tpw, stopper, err := NewMiddleware(testConfig, testEngineOpts, nil, util_log.Logger, fakeLimits{
		maxQueryLookback:    1 * time.Hour,
		maxQueryParallelism: 1,
	}, config.SchemaConfig{
		Configs: testSchemas,
	}, nil, false, nil, constants.Loki)
	if stopper != nil {
		defer stopper.Stop()
	}
	require.NoError(t, err)

	lreq := &LokiRequest{
		Query:     `{app="foo"} |= "foo"`,
		Limit:     10000,
		StartTs:   testTime.Add(-6 * time.Hour),
		EndTs:     testTime,
		Direction: logproto.FORWARD,
		Path:      "/loki/api/v1/query_range",
		Plan: &plan.QueryPlan{
			AST: syntax.MustParseExpr(`{app="foo"} |= "foo"`),
		},
	}

	ctx := user.InjectOrgID(context.Background(), "1")

	called := false
	h := queryrangebase.HandlerFunc(func(context.Context, queryrangebase.Request) (queryrangebase.Response, error) {
		called = true
		return nil, nil
	})

	resp, err := tpw.Wrap(h).Do(ctx, lreq)
	require.NoError(t, err)
	require.False(t, called)
	require.Equal(t, resp.(*LokiResponse).Status, "success")
}

func Test_MaxQueryLookBack_Types(t *testing.T) {
	m := NewLimitsMiddleware(fakeLimits{
		maxQueryLookback:    1 * time.Hour,
		maxQueryParallelism: 1,
	})

	now := time.Now()
	type tcase struct {
		request          queryrangebase.Request
		expectedResponse queryrangebase.Response
	}
	cases := []tcase{
		{
			request: &logproto.IndexStatsRequest{
				From:    model.Time(now.UnixMilli()),
				Through: model.Time(now.Add(-90 * time.Minute).UnixMilli()),
			},
			expectedResponse: &IndexStatsResponse{},
		},
		{
			request: &logproto.VolumeRequest{
				From:    model.Time(now.UnixMilli()),
				Through: model.Time(now.Add(-90 * time.Minute).UnixMilli()),
			},
			expectedResponse: &VolumeResponse{},
		},
	}

	ctx := user.InjectOrgID(context.Background(), "1")

	h := queryrangebase.HandlerFunc(func(context.Context, queryrangebase.Request) (queryrangebase.Response, error) {
		return nil, nil
	})

	for _, tcase := range cases {
		resp, err := m.Wrap(h).Do(ctx, tcase.request)
		require.NoError(t, err)

		require.Equal(t, reflect.TypeOf(tcase.expectedResponse), reflect.TypeOf(resp))
	}
}

func Test_GenerateCacheKey_NoDivideZero(t *testing.T) {
	l := cacheKeyLimits{WithSplitByLimits(nil, 0), nil, nil}
	start := time.Now()
	r := &LokiRequest{
		Query:   "qry",
		StartTs: start,
		Step:    int64(time.Minute / time.Millisecond),
	}

	require.Equal(
		t,
		fmt.Sprintf("foo:qry:%d:0:0", r.GetStep()),
		l.GenerateCacheKey(context.Background(), "foo", r),
	)
}

func Test_WeightedParallelism(t *testing.T) {
	limits := &fakeLimits{
		tsdbMaxQueryParallelism: 2048,
		maxQueryParallelism:     10,
	}

	for _, cfgs := range []struct {
		desc    string
		periods string
	}{
		{
			desc: "end configs",
			periods: `
- from: "2022-01-01"
  store: boltdb-shipper
  object_store: gcs
  schema: v12
- from: "2022-01-02"
  store: tsdb
  object_store: gcs
  schema: v12
`,
		},
		{
			// Add another test that wraps the tested period configs with other unused configs
			// to ensure we bounds-test properly
			desc: "middle configs",
			periods: `
- from: "2021-01-01"
  store: boltdb-shipper
  object_store: gcs
  schema: v12
- from: "2022-01-01"
  store: boltdb-shipper
  object_store: gcs
  schema: v12
- from: "2022-01-02"
  store: tsdb
  object_store: gcs
  schema: v12
- from: "2023-01-02"
  store: tsdb
  object_store: gcs
  schema: v12
`,
		},
	} {
		var confs []config.PeriodConfig
		require.Nil(t, yaml.Unmarshal([]byte(cfgs.periods), &confs))
		parsed, err := time.Parse("2006-01-02", "2022-01-02")
		borderTime := model.TimeFromUnix(parsed.Unix())
		require.Nil(t, err)

		for _, tc := range []struct {
			desc       string
			start, end model.Time
			exp        int
		}{
			{
				desc:  "50% each",
				start: borderTime.Add(-time.Hour),
				end:   borderTime.Add(time.Hour),
				exp:   1029,
			},
			{
				desc:  "75/25 split",
				start: borderTime.Add(-3 * time.Hour),
				end:   borderTime.Add(time.Hour),
				exp:   519,
			},
			{
				desc:  "start==end",
				start: borderTime.Add(time.Hour),
				end:   borderTime.Add(time.Hour),
				exp:   2048,
			},
			{
				desc:  "huge range to make sure we don't int overflow in the calculations.",
				start: borderTime,
				end:   borderTime.Add(24 * time.Hour * 365 * 20),
				exp:   2048,
			},
		} {
			t.Run(cfgs.desc+tc.desc, func(t *testing.T) {
				require.Equal(t, tc.exp, WeightedParallelism(context.Background(), confs, "fake", limits, tc.start, tc.end))
			})
		}
	}
}

func Test_WeightedParallelism_DivideByZeroError(t *testing.T) {
	t.Run("query end before start", func(t *testing.T) {
		parsed, err := time.Parse("2006-01-02", "2022-01-02")
		require.NoError(t, err)
		borderTime := model.TimeFromUnix(parsed.Unix())

		confs := []config.PeriodConfig{
			{
				From: config.DayTime{
					Time: borderTime.Add(-1 * time.Hour),
				},
				IndexType: types.TSDBType,
			},
		}

		result := WeightedParallelism(context.Background(), confs, "fake", &fakeLimits{tsdbMaxQueryParallelism: 50}, borderTime, borderTime.Add(-1*time.Hour))
		require.Equal(t, 1, result)
	})

	t.Run("negative start and end time", func(t *testing.T) {
		parsed, err := time.Parse("2006-01-02", "2022-01-02")
		require.NoError(t, err)
		borderTime := model.TimeFromUnix(parsed.Unix())

		confs := []config.PeriodConfig{
			{
				From: config.DayTime{
					Time: borderTime.Add(-1 * time.Hour),
				},
				IndexType: types.TSDBType,
			},
		}

		result := WeightedParallelism(context.Background(), confs, "fake", &fakeLimits{maxQueryParallelism: 50}, -100, -50)
		require.Equal(t, 1, result)
	})

	t.Run("query start and end time before config start", func(t *testing.T) {
		parsed, err := time.Parse("2006-01-02", "2022-01-02")
		require.NoError(t, err)
		borderTime := model.TimeFromUnix(parsed.Unix())

		confs := []config.PeriodConfig{
			{
				From: config.DayTime{
					Time: borderTime.Add(-1 * time.Hour),
				},
				IndexType: types.TSDBType,
			},
		}

		result := WeightedParallelism(context.Background(), confs, "fake", &fakeLimits{maxQueryParallelism: 50}, confs[0].From.Add(-24*time.Hour), confs[0].From.Add(-12*time.Hour))
		require.Equal(t, 1, result)
	})
}

func Test_MaxQuerySize(t *testing.T) {
	const statsBytes = 1000

	schemas := []config.PeriodConfig{
		{
			// BoltDB -> Time -4 days
			From:      config.DayTime{Time: model.TimeFromUnix(testTime.Add(-96 * time.Hour).Unix())},
			IndexType: types.BoltDBShipperType,
		},
		{
			// TSDB -> Time -2 days
			From:      config.DayTime{Time: model.TimeFromUnix(testTime.Add(-48 * time.Hour).Unix())},
			IndexType: types.TSDBType,
		},
	}

	for _, tc := range []struct {
		desc       string
		schema     string
		query      string
		queryRange time.Duration
		queryStart time.Time
		queryEnd   time.Time
		limits     Limits

		shouldErr                bool
		expectedQueryStatsHits   int
		expectedQuerierStatsHits int
	}{
		{
			desc:       "No TSDB",
			schema:     types.BoltDBShipperType,
			query:      `{app="foo"} |= "foo"`,
			queryRange: 1 * time.Hour,

			queryStart: testTime.Add(-96 * time.Hour),
			queryEnd:   testTime.Add(-90 * time.Hour),
			limits: fakeLimits{
				maxQueryBytesRead:   1,
				maxQuerierBytesRead: 1,
			},

			shouldErr:                false,
			expectedQueryStatsHits:   0,
			expectedQuerierStatsHits: 0,
		},
		{
			desc:       "Unlimited",
			query:      `{app="foo"} |= "foo"`,
			queryStart: testTime.Add(-48 * time.Hour),
			queryEnd:   testTime,
			limits: fakeLimits{
				maxQueryBytesRead:   0,
				maxQuerierBytesRead: 0,
			},

			shouldErr:                false,
			expectedQueryStatsHits:   0,
			expectedQuerierStatsHits: 0,
		},
		{
			desc:       "1 hour range",
			query:      `{app="foo"} |= "foo"`,
			queryStart: testTime.Add(-1 * time.Hour),
			queryEnd:   testTime,
			limits: fakeLimits{
				maxQueryBytesRead:   statsBytes,
				maxQuerierBytesRead: statsBytes,
			},

			shouldErr: false,
			// [testTime-1h, testTime)
			expectedQueryStatsHits:   1,
			expectedQuerierStatsHits: 1,
		},
		{
			desc:       "Query size too big",
			query:      `{app="foo"} |= "foo"`,
			queryStart: testTime.Add(-1 * time.Hour),
			queryEnd:   testTime,
			limits: fakeLimits{
				maxQueryBytesRead:   statsBytes - 1,
				maxQuerierBytesRead: statsBytes,
			},

			shouldErr:                true,
			expectedQueryStatsHits:   1,
			expectedQuerierStatsHits: 0,
		},
		{
			desc:       "Querier size too big",
			query:      `{app="foo"} |= "foo"`,
			queryStart: testTime.Add(-1 * time.Hour),
			queryEnd:   testTime,
			limits: fakeLimits{
				maxQueryBytesRead:   statsBytes,
				maxQuerierBytesRead: statsBytes - 1,
			},

			shouldErr:                true,
			expectedQueryStatsHits:   1,
			expectedQuerierStatsHits: 1,
		},
		{
			desc:       "Multi-matchers with offset",
			query:      `sum_over_time ({app="foo"} |= "foo" | unwrap foo [5m] ) / sum_over_time ({app="bar"} |= "bar" | unwrap bar [5m] offset 1h)`,
			queryStart: testTime.Add(-1 * time.Hour),
			queryEnd:   testTime,
			limits: fakeLimits{
				maxQueryBytesRead:   statsBytes,
				maxQuerierBytesRead: statsBytes,
			},

			shouldErr: false,
			// *2 since we have two matcher groups
			expectedQueryStatsHits:   1 * 2,
			expectedQuerierStatsHits: 1 * 2,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			queryStatsHits, queryStatsHandler := indexStatsResult(logproto.IndexStatsResponse{Bytes: uint64(statsBytes / max(tc.expectedQueryStatsHits, 1))})

			querierStatsHits, querierStatsHandler := indexStatsResult(logproto.IndexStatsResponse{Bytes: uint64(statsBytes / max(tc.expectedQuerierStatsHits, 1))})

			_, promHandler := promqlResult(matrix)

			lokiReq := &LokiRequest{
				Query:     tc.query,
				Limit:     1000,
				StartTs:   tc.queryStart,
				EndTs:     tc.queryEnd,
				Direction: logproto.FORWARD,
				Path:      "/query_range",
				Plan: &plan.QueryPlan{
					AST: syntax.MustParseExpr(tc.query),
				},
			}

			ctx := user.InjectOrgID(context.Background(), "foo")

			middlewares := []queryrangebase.Middleware{
				NewQuerySizeLimiterMiddleware(schemas, testEngineOpts, util_log.Logger, tc.limits, queryStatsHandler),
				NewQuerierSizeLimiterMiddleware(schemas, testEngineOpts, util_log.Logger, tc.limits, querierStatsHandler),
			}

			_, err := queryrangebase.MergeMiddlewares(middlewares...).Wrap(promHandler).Do(ctx, lokiReq)

			if tc.shouldErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, tc.expectedQueryStatsHits, *queryStatsHits)
			require.Equal(t, tc.expectedQuerierStatsHits, *querierStatsHits)
		})
	}
}

func Test_MaxQuerySize_MaxLookBackPeriod(t *testing.T) {
	engineOpts := testEngineOpts
	engineOpts.MaxLookBackPeriod = 1 * time.Hour

	lim := fakeLimits{
		maxQueryBytesRead:   1 << 10,
		maxQuerierBytesRead: 1 << 10,
	}

	statsHandler := queryrangebase.HandlerFunc(func(_ context.Context, req queryrangebase.Request) (queryrangebase.Response, error) {
		// This is the actual check that we're testing.
		require.Equal(t, testTime.Add(-engineOpts.MaxLookBackPeriod).UnixMilli(), req.GetStart().UnixMilli())

		return &IndexStatsResponse{
			Response: &logproto.IndexStatsResponse{
				Bytes: 1 << 10,
			},
		}, nil
	})

	for _, tc := range []struct {
		desc       string
		middleware queryrangebase.Middleware
	}{
		{
			desc:       "QuerySizeLimiter",
			middleware: NewQuerySizeLimiterMiddleware(testSchemasTSDB, engineOpts, util_log.Logger, lim, statsHandler),
		},
		{
			desc:       "QuerierSizeLimiter",
			middleware: NewQuerierSizeLimiterMiddleware(testSchemasTSDB, engineOpts, util_log.Logger, lim, statsHandler),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			lokiReq := &LokiInstantRequest{
				Query:     `{cluster="dev-us-central-0"}`,
				Limit:     1000,
				TimeTs:    testTime,
				Direction: logproto.FORWARD,
				Path:      "/loki/api/v1/query",
			}

			handler := tc.middleware.Wrap(
				queryrangebase.HandlerFunc(func(_ context.Context, _ queryrangebase.Request) (queryrangebase.Response, error) {
					return &LokiResponse{}, nil
				}),
			)

			ctx := user.InjectOrgID(context.Background(), "foo")
			_, err := handler.Do(ctx, lokiReq)
			require.NoError(t, err)
		})
	}
}

func TestAcquireWithTiming(t *testing.T) {
	ctx := context.Background()
	sem := NewSemaphoreWithTiming(2)

	// Channel to collect waiting times
	waitingTimes := make(chan struct {
		GoroutineID int
		WaitingTime time.Duration
	}, 3)

	tryAcquire := func(n int64, goroutineID int) {
		elapsed, err := sem.Acquire(ctx, n)
		if err != nil {
			t.Errorf("Expected no error, got %v", err)
		}
		waitingTimes <- struct {
			GoroutineID int
			WaitingTime time.Duration
		}{goroutineID, elapsed}

		defer sem.sem.Release(n)

		time.Sleep(10 * time.Millisecond)
	}

	go tryAcquire(1, 1)
	go tryAcquire(1, 2)

	// Sleep briefly to allow the first two goroutines to start running
	time.Sleep(5 * time.Millisecond)

	go tryAcquire(1, 3)

	// Collect and sort waiting times
	var waitingDurations []struct {
		GoroutineID int
		WaitingTime time.Duration
	}
	for i := 0; i < 3; i++ {
		waitingDurations = append(waitingDurations, <-waitingTimes)
	}
	// Find and check the waiting time for the third goroutine
	var waiting3 time.Duration
	for _, waiting := range waitingDurations {
		if waiting.GoroutineID == 3 {
			waiting3 = waiting.WaitingTime
			break
		}
	}

	// Check that the waiting time for the third request is larger than 0 milliseconds and less than 10 milliseconds
	require.Greater(t, waiting3, 0*time.Nanosecond)
	require.Less(t, waiting3, 10*time.Millisecond)
}
