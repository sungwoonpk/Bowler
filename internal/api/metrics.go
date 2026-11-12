package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/sungwoonpk/bowler/internal/domain"
)

// Metrics in the Prometheus text format.

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) error {
	counts, err := s.store.CountByState(r.Context())
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# HELP bowler_deliveries Deliveries by state.\n# TYPE bowler_deliveries gauge\n")
	for _, st := range []domain.State{domain.Pending, domain.InFlight, domain.Succeeded, domain.Failed, domain.Cancelled} {
		fmt.Fprintf(&b, "bowler_deliveries{state=%q} %d\n", string(st), counts[st])
	}
	if s.stats != nil {
		st := s.stats()
		b.WriteString("# HELP bowler_attempts_in_flight Attempts being made right now.\n# TYPE bowler_attempts_in_flight gauge\n")
		fmt.Fprintf(&b, "bowler_attempts_in_flight %d\n", st.InFlight)
		b.WriteString("# HELP bowler_attempts_total Attempts finished, by result.\n# TYPE bowler_attempts_total counter\n")
		fmt.Fprintf(&b, "bowler_attempts_total{result=\"succeeded\"} %d\nbowler_attempts_total{result=\"retried\"} %d\n", st.Succeeded, st.Retried)
		fmt.Fprintf(&b, "bowler_attempts_total{result=\"failed\"} %d\nbowler_attempts_total{result=\"aborted\"} %d\n", st.Failed, st.Aborted)
		b.WriteString("# HELP bowler_claimed_total Deliveries taken from the queue.\n# TYPE bowler_claimed_total counter\n")
		fmt.Fprintf(&b, "bowler_claimed_total %d\n", st.Claimed)
		b.WriteString("# HELP bowler_lease_lost_total Results that arrived after the lease had been given to another worker.\n# TYPE bowler_lease_lost_total counter\n")
		fmt.Fprintf(&b, "bowler_lease_lost_total %d\n", st.LeaseLost)
		b.WriteString("# HELP bowler_store_errors_total Database errors while running the queue.\n# TYPE bowler_store_errors_total counter\n")
		fmt.Fprintf(&b, "bowler_store_errors_total %d\n", st.StoreError)
		b.WriteString("# HELP bowler_sender_panics_total Panics recovered while sending.\n# TYPE bowler_sender_panics_total counter\n")
		fmt.Fprintf(&b, "bowler_sender_panics_total %d\n", st.Panics)
		b.WriteString("# HELP bowler_result_batches_total Database transactions used to record attempt results.\n# TYPE bowler_result_batches_total counter\n")
		fmt.Fprintf(&b, "bowler_result_batches_total %d\n", st.ResultBatches)
		b.WriteString("# HELP bowler_results_total Attempt results recorded. Divided by the batches, the average batch size.\n# TYPE bowler_results_total counter\n")
		fmt.Fprintf(&b, "bowler_results_total %d\n", st.Results)
	}
	if s.retention != nil {
		st := s.retention()
		b.WriteString("# HELP bowler_retention_deleted_total Rows deleted because they were older than the retention period.\n# TYPE bowler_retention_deleted_total counter\n")
		fmt.Fprintf(&b, "bowler_retention_deleted_total{kind=\"messages\"} %d\nbowler_retention_deleted_total{kind=\"deliveries\"} %d\n", st.Messages, st.Deliveries)
		fmt.Fprintf(&b, "bowler_retention_deleted_total{kind=\"attempts\"} %d\nbowler_retention_deleted_total{kind=\"endpoints\"} %d\n", st.Attempts, st.Endpoints)
		fmt.Fprintf(&b, "bowler_retention_deleted_total{kind=\"audit_entries\"} %d\n", st.AuditEntries)
		b.WriteString("# HELP bowler_retention_errors_total Retention sweeps that failed.\n# TYPE bowler_retention_errors_total counter\n")
		fmt.Fprintf(&b, "bowler_retention_errors_total %d\n", st.Errors)
	}
	b.WriteString("# HELP bowler_audit_errors_total Audit entries that could not be recorded.\n# TYPE bowler_audit_errors_total counter\n")
	fmt.Fprintf(&b, "bowler_audit_errors_total %d\n", s.auditErrors.Load())
	b.WriteString("# HELP bowler_build_info The running version.\n# TYPE bowler_build_info gauge\n")
	fmt.Fprintf(&b, "bowler_build_info{version=%q} 1\n", s.version)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
	return nil
}
