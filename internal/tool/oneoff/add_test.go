package oneoff

import "testing"

func TestAdd(t *testing.T) {
	tests := []struct {
		name     string
		a, b     int
		expected int
	}{
		{"simple_positive", 2, 3, 5},
		{"zero_and_value", 0, 10, 10},
		{"negative_and_positive", -5, 3, -2},
		{"both_negative", -1, -1, -2},
		{"large_values", 999999, 888888, 1888887},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := Add(tc.a, tc.b)
			if result != tc.expected {
				t.Errorf("Add(%d, %d) = %d; expected %d", tc.a, tc.b, result, tc.expected)
			}
		})
	}
}
