package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// RunnerValue labels the shared runner job; slugs never hold "_".
const RunnerValue = "_runner"

var (
	tableRE = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_]+\.[A-Za-z0-9_*]+$`)
	labelRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
)

// Export reads Google Cloud cost from the Cloud Billing export in BigQuery.
// Resources are attributed by one label: Label=<slug> for an app's own,
// Label=_runner for the runner job.
type Export struct {
	API     string // "https://bigquery.googleapis.com", overridable in tests
	Project string // runs the query: needs bigquery.jobUser
	Table   string // project.dataset.gcp_billing_export_v1_*
	Label   string
	Client  *http.Client // authorized for BigQuery
}

// Validate checks the names spliced into the query.
func (e *Export) Validate() error {
	if !tableRE.MatchString(e.Table) {
		return fmt.Errorf("billing table %q: want project.dataset.table", e.Table)
	}
	if !labelRE.MatchString(e.Label) {
		return fmt.Errorf("billing label %q: want a label key (a-z, 0-9, _ and -, starting with a letter)", e.Label)
	}
	if e.Project == "" {
		return errors.New("billing project not set")
	}
	return nil
}

// Cost is the month's net cost (credits included) by label value: slugs and RunnerValue.
func (e *Export) Cost(ctx context.Context, month time.Time) (map[string]float64, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	q := fmt.Sprintf("SELECT l.value, SUM(cost) + SUM(IFNULL((SELECT SUM(c.amount) FROM UNNEST(credits) c), 0))\n"+
		"FROM `%s`, UNNEST(labels) l\n"+
		"WHERE invoice.month = @month AND l.key = @label\n"+
		"GROUP BY 1", e.Table)
	body, _ := json.Marshal(map[string]any{
		"query":         q,
		"useLegacySql":  false,
		"timeoutMs":     60000,
		"parameterMode": "NAMED",
		"queryParameters": []any{
			param("month", Month(month).Format("200601")),
			param("label", e.Label),
		},
	})
	api := e.API
	if api == "" {
		api = "https://bigquery.googleapis.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api+"/bigquery/v2/projects/"+e.Project+"/queries", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		JobComplete bool `json:"jobComplete"`
		PageToken   string
		Rows        []struct {
			F []struct {
				V *string `json:"v"`
			} `json:"f"`
		} `json:"rows"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("billing export: %s: %w", resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("billing export: %s: %s", resp.Status, out.Error.Message)
	}
	if !out.JobComplete {
		return nil, errors.New("billing export: query didn't finish in time")
	}
	if out.PageToken != "" {
		return nil, errors.New("billing export: more rows than one page")
	}
	cost := map[string]float64{}
	for _, r := range out.Rows {
		if len(r.F) != 2 || r.F[0].V == nil || r.F[1].V == nil {
			continue
		}
		v, err := strconv.ParseFloat(*r.F[1].V, 64)
		if err != nil {
			return nil, fmt.Errorf("billing export: cost %q: %w", *r.F[1].V, err)
		}
		cost[*r.F[0].V] += v
	}
	return cost, nil
}

func param(name, value string) map[string]any {
	return map[string]any{
		"name":           name,
		"parameterType":  map[string]string{"type": "STRING"},
		"parameterValue": map[string]string{"value": value},
	}
}
