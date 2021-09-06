// Copyright Sam Xie
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

package otelsql

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"go.opentelemetry.io/otel/trace"
)

type mockConn struct {
	shouldError bool

	resetSessionCount int
	resetSessionCtx   context.Context

	beginTxCount int
	beginTxCtx   context.Context

	prepareContextCount int
	prepareContextCtx   context.Context
	prepareContextQuery string

	queryContextCount int
	queryContextCtx   context.Context
	queryContextQuery string

	execContextCount int
	execContextCtx   context.Context
	execContextQuery string

	pingCount int
	pingCtx   context.Context
}

func newMockConn(shouldError bool) *mockConn {
	return &mockConn{shouldError: shouldError}
}

func (m *mockConn) ResetSession(ctx context.Context) error {
	m.resetSessionCtx = ctx
	m.resetSessionCount++
	if m.shouldError {
		return errors.New("resetSession")
	}
	return nil
}

func (m *mockConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	m.beginTxCount++
	m.beginTxCtx = ctx
	if m.shouldError {
		return nil, errors.New("beginTx")
	}
	return newMockTx(false), nil
}

func (m *mockConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	m.prepareContextCount++
	m.prepareContextCtx = ctx
	m.prepareContextQuery = query
	if m.shouldError {
		return nil, errors.New("prepareContext")
	}
	return newMockStmt(false), nil
}

func (m *mockConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	m.queryContextCount++
	m.queryContextCtx = ctx
	m.queryContextQuery = query
	if m.shouldError {
		return nil, errors.New("queryContext")
	}
	return newMockRows(false), nil
}

func (m *mockConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	m.execContextCount++
	m.execContextCtx = ctx
	m.execContextQuery = query
	if m.shouldError {
		return nil, errors.New("execContext")
	}
	return nil, nil
}

func (m *mockConn) Ping(ctx context.Context) error {
	m.pingCount++
	m.pingCtx = ctx
	if m.shouldError {
		return errors.New("execContext")
	}
	return nil
}

func (m *mockConn) Prepare(query string) (driver.Stmt, error) {
	return newMockStmt(false), nil
}

func (m *mockConn) Close() error {
	return nil
}

func (m *mockConn) Begin() (driver.Tx, error) {
	return newMockTx(false), nil
}

var (
	_ driver.Pinger             = (*mockConn)(nil)
	_ driver.ExecerContext      = (*mockConn)(nil)
	_ driver.QueryerContext     = (*mockConn)(nil)
	_ driver.Conn               = (*mockConn)(nil)
	_ driver.ConnPrepareContext = (*mockConn)(nil)
	_ driver.ConnBeginTx        = (*mockConn)(nil)
	_ driver.SessionResetter    = (*mockConn)(nil)
)

func TestOtConn_Ping(t *testing.T) {
	testCases := []struct {
		name            string
		error           bool
		pingOption      bool
		allowRootOption bool
		noParentSpan    bool
	}{
		{
			name:       "ping enabled",
			pingOption: true,
		},
		{
			name:            "ping enabled with no parent span, allow root span",
			pingOption:      true,
			allowRootOption: true,
			noParentSpan:    true,
		},
		{
			name:            "ping enabled with no parent span, disallow root span",
			pingOption:      true,
			allowRootOption: false,
			noParentSpan:    true,
		},
		{
			name:       "ping enabled with error",
			pingOption: true,
			error:      true,
		},
		{
			name: "ping disabled",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Prepare traces
			ctx, sr, tracer, dummySpan := prepareTraces(tc.noParentSpan)

			// New conn
			cfg := newMockConfig(tracer)
			cfg.SpanOptions.Ping = tc.pingOption
			cfg.SpanOptions.AllowRoot = tc.allowRootOption
			mc := newMockConn(tc.error)
			otelConn := newConn(mc, cfg)

			err := otelConn.Ping(ctx)
			if tc.error {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			spanList := sr.Ended()
			if tc.pingOption {
				expectedSpanCount := getExpectedSpanCount(tc.allowRootOption, tc.noParentSpan)
				// One dummy span and one span created in Ping
				require.Equal(t, expectedSpanCount, len(spanList))

				if tc.pingOption {
					assertSpanList(t, spanList, spanAssertionParameter{
						parentSpan:         dummySpan,
						error:              tc.error,
						expectedAttributes: cfg.Attributes,
						expectedMethod:     MethodConnPing,
						allowRootOption:    tc.allowRootOption,
						noParentSpan:       tc.noParentSpan,
						ctx:                mc.pingCtx,
					})

					assert.Equal(t, 1, mc.pingCount)
				}
			} else {
				if !tc.noParentSpan {
					require.Equal(t, 1, len(spanList))
				}
			}
		})
	}
}

func TestOtConn_ExecContext(t *testing.T) {
	expectedAttrs := []attribute.KeyValue{semconv.DBStatementKey.String("query")}
	testCases := []struct {
		name            string
		error           bool
		allowRootOption bool
		noParentSpan    bool
		disableQuery    bool
		attrs           []attribute.KeyValue
	}{
		{
			name:  "no error",
			attrs: expectedAttrs,
		},
		{
			name:         "no query db.statement",
			disableQuery: true,
		},
		{
			name:  "with error",
			error: true,
			attrs: expectedAttrs,
		},
		{
			name:         "no parent span, disallow root span",
			noParentSpan: true,
			attrs:        expectedAttrs,
		},
		{
			name:            "no parent span, allow root span",
			noParentSpan:    true,
			allowRootOption: true,
			attrs:           expectedAttrs,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Prepare traces
			ctx, sr, tracer, dummySpan := prepareTraces(tc.noParentSpan)

			// New conn
			cfg := newMockConfig(tracer)
			cfg.SpanOptions.AllowRoot = tc.allowRootOption
			cfg.SpanOptions.DisableQuery = tc.disableQuery
			mc := newMockConn(tc.error)
			otelConn := newConn(mc, cfg)

			_, err := otelConn.ExecContext(ctx, "query", nil)
			if tc.error {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			spanList := sr.Ended()
			expectedSpanCount := getExpectedSpanCount(tc.allowRootOption, tc.noParentSpan)
			// One dummy span and one span created in ExecContext
			require.Equal(t, expectedSpanCount, len(spanList))

			assertSpanList(t, spanList, spanAssertionParameter{
				parentSpan:         dummySpan,
				error:              tc.error,
				expectedAttributes: append(cfg.Attributes, tc.attrs...),
				expectedMethod:     MethodConnExec,
				allowRootOption:    tc.allowRootOption,
				noParentSpan:       tc.noParentSpan,
				ctx:                mc.execContextCtx,
			})

			assert.Equal(t, 1, mc.execContextCount)
			assert.Equal(t, "query", mc.execContextQuery)
		})
	}
}

func TestOtConn_QueryContext(t *testing.T) {
	expectedAttrs := []attribute.KeyValue{semconv.DBStatementKey.String("query")}
	testCases := []struct {
		name            string
		error           bool
		allowRootOption bool
		noParentSpan    bool
		disableQuery    bool
		attrs           []attribute.KeyValue
	}{
		{
			name:  "no error",
			attrs: expectedAttrs,
		},
		{
			name:         "no query db.statement",
			disableQuery: true,
		},
		{
			name:  "with error",
			error: true,
			attrs: expectedAttrs,
		},
		{
			name:         "no parent span, disallow root span",
			noParentSpan: true,
			attrs:        expectedAttrs,
		},
		{
			name:            "no parent span, allow root span",
			noParentSpan:    true,
			allowRootOption: true,
			attrs:           expectedAttrs,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Prepare traces
			ctx, sr, tracer, dummySpan := prepareTraces(tc.noParentSpan)

			// New conn
			cfg := newMockConfig(tracer)
			cfg.SpanOptions.AllowRoot = tc.allowRootOption
			cfg.SpanOptions.DisableQuery = tc.disableQuery
			mc := newMockConn(tc.error)
			otelConn := newConn(mc, cfg)

			rows, err := otelConn.QueryContext(ctx, "query", nil)
			if tc.error {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			spanList := sr.Ended()
			expectedSpanCount := getExpectedSpanCount(tc.allowRootOption, tc.noParentSpan)
			// One dummy span and one span created in QueryContext
			require.Equal(t, expectedSpanCount, len(spanList))

			assertSpanList(t, spanList, spanAssertionParameter{
				parentSpan:         dummySpan,
				error:              tc.error,
				expectedAttributes: append(cfg.Attributes, tc.attrs...),
				expectedMethod:     MethodConnQuery,
				allowRootOption:    tc.allowRootOption,
				noParentSpan:       tc.noParentSpan,
				ctx:                mc.queryContextCtx,
			})

			assert.Equal(t, 1, mc.queryContextCount)
			assert.Equal(t, "query", mc.queryContextQuery)

			if !tc.error {
				otelRows, ok := rows.(*otRows)
				require.True(t, ok)
				if dummySpan != nil {
					assert.Equal(t, dummySpan.SpanContext().TraceID(), otelRows.span.SpanContext().TraceID())

					// Get a span from started span list
					startedSpanList := sr.Started()
					require.Len(t, startedSpanList, expectedSpanCount+1)
					span := startedSpanList[expectedSpanCount]
					// Make sure this span is the same as the span from otelRows
					require.Equal(t, otelRows.span.SpanContext().SpanID(), span.SpanContext().SpanID())

					// The span that creates in newRows() is the child of the dummySpan
					assert.Equal(t, dummySpan.SpanContext().SpanID(), span.Parent().SpanID())
				}
			}
		})
	}
}

func TestOtConn_PrepareContext(t *testing.T) {
	expectedAttrs := []attribute.KeyValue{semconv.DBStatementKey.String("query")}
	testCases := []struct {
		name            string
		error           bool
		allowRootOption bool
		noParentSpan    bool
		disableQuery    bool
		attrs           []attribute.KeyValue
	}{
		{
			name:  "no error",
			attrs: expectedAttrs,
		},
		{
			name:         "no query db.statement",
			disableQuery: true,
		},
		{
			name:  "with error",
			error: true,
			attrs: expectedAttrs,
		},
		{
			name:         "no parent span, disallow root span",
			noParentSpan: true,
			attrs:        expectedAttrs,
		},
		{
			name:            "no parent span, allow root span",
			noParentSpan:    true,
			allowRootOption: true,
			attrs:           expectedAttrs,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Prepare traces
			ctx, sr, tracer, dummySpan := prepareTraces(tc.noParentSpan)

			// New conn
			cfg := newMockConfig(tracer)
			cfg.SpanOptions.AllowRoot = tc.allowRootOption
			cfg.SpanOptions.DisableQuery = tc.disableQuery
			mc := newMockConn(tc.error)
			otelConn := newConn(mc, cfg)

			stmt, err := otelConn.PrepareContext(ctx, "query")
			if tc.error {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			spanList := sr.Ended()
			expectedSpanCount := getExpectedSpanCount(tc.allowRootOption, tc.noParentSpan)
			// One dummy span and one span created in PrepareContext
			require.Equal(t, expectedSpanCount, len(spanList))

			assertSpanList(t, spanList, spanAssertionParameter{
				parentSpan:         dummySpan,
				error:              tc.error,
				expectedAttributes: append(cfg.Attributes, tc.attrs...),
				expectedMethod:     MethodConnPrepare,
				allowRootOption:    tc.allowRootOption,
				noParentSpan:       tc.noParentSpan,
				ctx:                mc.prepareContextCtx,
			})

			assert.Equal(t, 1, mc.prepareContextCount)
			assert.Equal(t, "query", mc.prepareContextQuery)

			if !tc.error {
				otelStmt, ok := stmt.(*otStmt)
				require.True(t, ok)
				assert.Equal(t, "query", otelStmt.query)
			}
		})
	}
}

func TestOtConn_BeginTx(t *testing.T) {
	testCases := []struct {
		name            string
		error           bool
		allowRootOption bool
		noParentSpan    bool
	}{
		{
			name: "no error",
		},
		{
			name:  "with error",
			error: true,
		},
		{
			name:         "no parent span, disallow root span",
			noParentSpan: true,
		},
		{
			name:            "no parent span, allow root span",
			noParentSpan:    true,
			allowRootOption: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Prepare traces
			ctx, sr, tracer, dummySpan := prepareTraces(tc.noParentSpan)

			// New conn
			cfg := newMockConfig(tracer)
			cfg.SpanOptions.AllowRoot = tc.allowRootOption
			mc := newMockConn(tc.error)
			otelConn := newConn(mc, cfg)

			tx, err := otelConn.BeginTx(ctx, driver.TxOptions{})
			if tc.error {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			spanList := sr.Ended()
			expectedSpanCount := getExpectedSpanCount(tc.allowRootOption, tc.noParentSpan)
			// One dummy span and one span created in BeginTx
			require.Equal(t, expectedSpanCount, len(spanList))

			assertSpanList(t, spanList, spanAssertionParameter{
				parentSpan:         dummySpan,
				error:              tc.error,
				expectedAttributes: cfg.Attributes,
				expectedMethod:     MethodConnBeginTx,
				allowRootOption:    tc.allowRootOption,
				noParentSpan:       tc.noParentSpan,
				ctx:                mc.beginTxCtx,
			})

			assert.Equal(t, 1, mc.beginTxCount)

			if !tc.error {
				otelTx, ok := tx.(*otTx)
				require.True(t, ok)

				if dummySpan != nil {
					assert.Equal(t, dummySpan.SpanContext(), trace.SpanContextFromContext(otelTx.ctx))
				}
			}
		})
	}
}

func TestOtConn_ResetSession(t *testing.T) {
	testCases := []struct {
		name            string
		error           bool
		allowRootOption bool
		noParentSpan    bool
	}{
		{
			name: "no error",
		},
		{
			name:  "with error",
			error: true,
		},
		{
			name:         "no parent span, disallow root span",
			noParentSpan: true,
		},
		{
			name:            "no parent span, allow root span",
			noParentSpan:    true,
			allowRootOption: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Prepare traces
			ctx, sr, tracer, dummySpan := prepareTraces(tc.noParentSpan)

			// New conn
			cfg := newMockConfig(tracer)
			cfg.SpanOptions.AllowRoot = tc.allowRootOption
			mc := newMockConn(tc.error)
			otelConn := newConn(mc, cfg)

			err := otelConn.ResetSession(ctx)
			if tc.error {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			spanList := sr.Ended()
			expectedSpanCount := getExpectedSpanCount(tc.allowRootOption, tc.noParentSpan)
			// One dummy span and one span created in ResetSession
			require.Equal(t, expectedSpanCount, len(spanList))

			assertSpanList(t, spanList, spanAssertionParameter{
				parentSpan:         dummySpan,
				error:              tc.error,
				expectedAttributes: cfg.Attributes,
				expectedMethod:     MethodConnResetSession,
				allowRootOption:    tc.allowRootOption,
				noParentSpan:       tc.noParentSpan,
				ctx:                mc.resetSessionCtx,
			})

			assert.Equal(t, 1, mc.resetSessionCount)
		})
	}
}
