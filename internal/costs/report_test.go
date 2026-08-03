package costs

import "testing"

func sampleInput() Input {
	return Input{
		GroupBy:  "SERVICE",
		Currency: "USD",
		Yesterday: Period{
			Start: "2026-08-02", End: "2026-08-03",
			ByGroup: map[string]float64{
				"Amazon EC2":        120.00,
				"Amazon S3":         15.50,
				"AWS Lambda":        2.00,
				"Amazon RDS":        40.00,
				"Amazon CloudWatch": 30.00, // the spike
				"Amazon Route 53":   0.50,
			},
		},
		DayBefore: Period{
			Start: "2026-08-01", End: "2026-08-02",
			ByGroup: map[string]float64{
				"Amazon EC2":        118.00,
				"Amazon S3":         16.00,
				"AWS Lambda":        2.00,
				"Amazon RDS":        41.00,
				"Amazon CloudWatch": 3.00,
				"AWS Glue":          9.00, // present yesterday-1, gone yesterday
			},
		},
		MTD: Period{
			Start: "2026-08-01", End: "2026-08-03",
			ByGroup: map[string]float64{
				"Amazon EC2": 238.00,
				"Amazon S3":  31.50,
			},
		},
	}
}

func TestBuildReportTotalsAndOrdering(t *testing.T) {
	r := BuildReport(sampleInput(), 3, 100)

	if got, want := r.Total, 208.00; got != want {
		t.Fatalf("total = %v, want %v", got, want)
	}
	if got, want := r.PrevTotal, 189.00; got != want {
		t.Fatalf("prev total = %v, want %v", got, want)
	}
	if got, want := r.MTDTotal, 269.50; got != want {
		t.Fatalf("mtd total = %v, want %v", got, want)
	}
	if len(r.Rows) != 3 {
		t.Fatalf("top-N not applied: got %d rows", len(r.Rows))
	}
	if r.Rows[0].Name != "Amazon EC2" || r.Rows[1].Name != "Amazon RDS" || r.Rows[2].Name != "Amazon CloudWatch" {
		t.Fatalf("ordering wrong: %+v", []string{r.Rows[0].Name, r.Rows[1].Name, r.Rows[2].Name})
	}
}

func TestArrowsAndDeltas(t *testing.T) {
	r := BuildReport(sampleInput(), 10, 100)
	by := map[string]Row{}
	for _, row := range r.Rows {
		by[row.Name] = row
	}

	if by["Amazon EC2"].Arrow != arrowUp {
		t.Errorf("EC2 rose, want ▲ got %q", by["Amazon EC2"].Arrow)
	}
	if by["Amazon RDS"].Arrow != arrowDown {
		t.Errorf("RDS fell, want ▼ got %q", by["Amazon RDS"].Arrow)
	}
	if by["AWS Lambda"].Arrow != arrowFlat {
		t.Errorf("Lambda flat, want ▬ got %q", by["AWS Lambda"].Arrow)
	}
	if got := by["Amazon RDS"].DoDDelta; got != -1.00 {
		t.Errorf("RDS delta = %v, want -1.00", got)
	}
}

func TestAnomalyDetection(t *testing.T) {
	r := BuildReport(sampleInput(), 10, 100)

	if len(r.Anomalies) != 1 {
		t.Fatalf("want exactly 1 anomaly, got %d: %+v", len(r.Anomalies), r.Anomalies)
	}
	a := r.Anomalies[0]
	if a.Name != "Amazon CloudWatch" {
		t.Fatalf("wrong anomaly: %q", a.Name)
	}
	if a.DoDPct != 900 {
		t.Fatalf("CloudWatch 3->30 is +900%%, got %v", a.DoDPct)
	}
	// A flat, tiny group must never be flagged even at a huge percentage.
	for _, row := range r.Rows {
		if row.Name == "AWS Lambda" && row.Anomaly {
			t.Fatalf("Lambda ($2->$2) should not be an anomaly")
		}
	}
}

func TestVanishedGroupSurfacedAsDrop(t *testing.T) {
	r := BuildReport(sampleInput(), 10, 100)
	var glue *Row
	for i := range r.Rows {
		if r.Rows[i].Name == "AWS Glue" {
			glue = &r.Rows[i]
		}
	}
	if glue == nil {
		t.Fatal("AWS Glue disappeared entirely; expected a -100% drop row")
	}
	if glue.Amount != 0 || glue.DoDPct != -100 || glue.Arrow != arrowDown {
		t.Fatalf("Glue drop row wrong: %+v", *glue)
	}
}

func TestPctChangeFromZero(t *testing.T) {
	if got := pctChange(5, 0); got != 100 {
		t.Errorf("new spend from zero should read +100%%, got %v", got)
	}
	if got := pctChange(0, 0); got != 0 {
		t.Errorf("0->0 should be 0%%, got %v", got)
	}
}

func TestMoneyAndPctFormatting(t *testing.T) {
	if got := Money("USD", 12.345); got != "$12.35" {
		t.Errorf("Money USD = %q", got)
	}
	if got := Money("SEK", 10.0); got != "10.00 SEK" {
		t.Errorf("Money SEK = %q", got)
	}
	if got := Money("USD", -0.0001); got != "$0.00" {
		t.Errorf("Money should not print -0.00, got %q", got)
	}
	if got := SignedPct(42.04); got != "+42.0%" {
		t.Errorf("SignedPct = %q", got)
	}
}
