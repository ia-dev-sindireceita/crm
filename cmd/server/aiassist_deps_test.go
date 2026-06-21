package main

// SIN-65474 — newAIAssistDeps surfaces the SummaryReader staleness port.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// fakeSummarizerReader implements both AssistSummarizer and
// AssistSummaryReader, mirroring the production *aiassistusecase.Service.
type fakeSummarizerReader struct{ fakeSummarizer }

func (fakeSummarizerReader) LatestSummaryGeneratedAt(context.Context, uuid.UUID, uuid.UUID) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

func TestNewAIAssistDeps_SurfacesSummaryReaderWhenSupported(t *testing.T) {
	deps := newAIAssistDeps(fakeSummarizerReader{})
	if deps.Summarizer == nil {
		t.Fatalf("Summarizer must be set")
	}
	if deps.SummaryReader == nil {
		t.Fatalf("SummaryReader must be surfaced when the summarizer implements it")
	}
}

func TestNewAIAssistDeps_NilReaderWhenUnsupported(t *testing.T) {
	deps := newAIAssistDeps(fakeSummarizer{})
	if deps.Summarizer == nil {
		t.Fatalf("Summarizer must be set")
	}
	if deps.SummaryReader != nil {
		t.Fatalf("SummaryReader must stay nil when the summarizer does not implement it")
	}
}

func TestNewAIAssistDeps_NilSummarizerKeepsFeatureOff(t *testing.T) {
	var nilSummarizer webinbox.AssistSummarizer
	deps := newAIAssistDeps(nilSummarizer)
	if deps.Summarizer != nil || deps.SummaryReader != nil {
		t.Fatalf("nil summarizer must leave both fields nil; got %+v", deps)
	}
}
