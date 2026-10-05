package request

import "testing"

func strPtr(s string) *string { return &s }
func i64Ptr(i int64) *int64   { return &i }

func TestCalculateCompleteness(t *testing.T) {
	cases := []struct {
		name string
		req  BuyerRequest
		want Completeness
	}{
		{
			name: "product, budget, and location present -> complete",
			req: BuyerRequest{
				Product:   strPtr("iPhone 13"),
				BudgetMax: i64Ptr(400000),
				Location:  strPtr("Benin City"),
			},
			want: CompletenessComplete,
		},
		{
			name: "product only -> partially complete",
			req: BuyerRequest{
				Product: strPtr("laptop"),
			},
			want: CompletenessPartial,
		},
		{
			name: "product and budget, no location -> partially complete",
			req: BuyerRequest{
				Product:   strPtr("laptop"),
				BudgetMax: i64Ptr(250000),
			},
			want: CompletenessPartial,
		},
		{
			name: "nothing identified -> insufficient",
			req:  BuyerRequest{},
			want: CompletenessInsufficient,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.req.CalculateCompleteness()
			if got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}
