package internal

import (
	"testing"
)

// TestRoutingPrecedence asserts the shape+qualify gates correctly classify
// cycles by DEX composition. This is the unit-level counterpart to the
// smoketest CONTRACT-* probes, which verify the deployed contracts accept
// the calldata each gate produces.
//
// Precedence (by evalOneCandidate and fastEvalCandidate):
//   V3Mini (V3-family only)
//   V4Mini (V4 only)
//   MixedV3V4 (mix of V3-family and V4)
//   generic (everything else — V2, Curve, Balancer, 6+ hops, etc.)
//
// The test covers each expected routing outcome plus boundary cases on
// hop count and DEX mixing.
func TestRoutingPrecedence(t *testing.T) {
	tA := NewToken("0x0000000000000000000000000000000000000001", "A", 18)
	tB := NewToken("0x0000000000000000000000000000000000000002", "B", 18)
	tC := NewToken("0x0000000000000000000000000000000000000003", "C", 18)

	mkPool := func(addr string, dex DEXType) *Pool {
		return &Pool{Address: addr, DEX: dex, Token0: tA, Token1: tB}
	}
	mkEdge := func(dex DEXType, in, out *Token) Edge {
		return Edge{Pool: mkPool("0x"+string(dex.String()[0])+in.Symbol+out.Symbol, dex), TokenIn: in, TokenOut: out}
	}

	flashV3 := FlashSelection{Source: FlashV3Pool, Available: true}
	flashBal := FlashSelection{Source: FlashBalancer, Available: true}

	cases := []struct {
		name      string
		edges     []Edge
		flash     FlashSelection
		wantV3    bool
		wantV4    bool
		wantMixed bool
	}{
		{
			name: "pure V3 2-hop via V3 flash → V3Mini",
			edges: []Edge{
				mkEdge(DEXUniswapV3, tA, tB),
				mkEdge(DEXSushiSwapV3, tB, tA),
			},
			flash:  flashV3,
			wantV3: true,
		},
		{
			name: "pure V4 2-hop via V3 flash → V4Mini",
			edges: []Edge{
				mkEdge(DEXUniswapV4, tA, tB),
				mkEdge(DEXUniswapV4, tB, tA),
			},
			flash:  flashV3,
			wantV4: true,
		},
		{
			name: "mixed V4+V3 3-hop via V3 flash → MixedV3V4",
			edges: []Edge{
				mkEdge(DEXUniswapV4, tA, tB),
				mkEdge(DEXUniswapV4, tB, tC),
				mkEdge(DEXUniswapV3, tC, tA),
			},
			flash:     flashV3,
			wantMixed: true,
		},
		{
			name: "mixed V3+V4 2-hop via V3 flash → MixedV3V4",
			edges: []Edge{
				mkEdge(DEXUniswapV3, tA, tB),
				mkEdge(DEXUniswapV4, tB, tA),
			},
			flash:     flashV3,
			wantMixed: true,
		},
		{
			name: "pure V3 via Balancer flash → none (mini requires V3 flash)",
			edges: []Edge{
				mkEdge(DEXUniswapV3, tA, tB),
				mkEdge(DEXUniswapV3, tB, tA),
			},
			flash: flashBal,
		},
		{
			name: "pure V4 via Balancer flash → none",
			edges: []Edge{
				mkEdge(DEXUniswapV4, tA, tB),
				mkEdge(DEXUniswapV4, tB, tA),
			},
			flash: flashBal,
		},
		{
			name: "mixed V3+V4 via Balancer flash → none",
			edges: []Edge{
				mkEdge(DEXUniswapV3, tA, tB),
				mkEdge(DEXUniswapV4, tB, tA),
			},
			flash: flashBal,
		},
		{
			name: "V3 + V2 mix → none (V2 kills shape for all minis)",
			edges: []Edge{
				mkEdge(DEXUniswapV3, tA, tB),
				mkEdge(DEXUniswapV2, tB, tA),
			},
			flash: flashV3,
		},
		{
			name: "V4 + V2 mix → none",
			edges: []Edge{
				mkEdge(DEXUniswapV4, tA, tB),
				mkEdge(DEXUniswapV2, tB, tA),
			},
			flash: flashV3,
		},
		{
			name: "V3+V4+V2 → none (any non-{V3,V4} hop disqualifies Mixed)",
			edges: []Edge{
				mkEdge(DEXUniswapV3, tA, tB),
				mkEdge(DEXUniswapV4, tB, tC),
				mkEdge(DEXUniswapV2, tC, tA),
			},
			flash: flashV3,
		},
		{
			name: "1-hop V3 → none (below min hops)",
			edges: []Edge{
				mkEdge(DEXUniswapV3, tA, tA),
			},
			flash: flashV3,
		},
		{
			name: "6-hop V4 → none (above max hops)",
			edges: []Edge{
				mkEdge(DEXUniswapV4, tA, tB),
				mkEdge(DEXUniswapV4, tB, tC),
				mkEdge(DEXUniswapV4, tC, tA),
				mkEdge(DEXUniswapV4, tA, tB),
				mkEdge(DEXUniswapV4, tB, tC),
				mkEdge(DEXUniswapV4, tC, tA),
			},
			flash: flashV3,
		},
		{
			name: "5-hop V4 → V4Mini (upper bound)",
			edges: []Edge{
				mkEdge(DEXUniswapV4, tA, tB),
				mkEdge(DEXUniswapV4, tB, tC),
				mkEdge(DEXUniswapV4, tC, tA),
				mkEdge(DEXUniswapV4, tA, tB),
				mkEdge(DEXUniswapV4, tB, tA),
			},
			flash:  flashV3,
			wantV4: true,
		},
		{
			name: "PancakeV3+V4 mixed → MixedV3V4",
			edges: []Edge{
				mkEdge(DEXPancakeV3, tA, tB),
				mkEdge(DEXUniswapV4, tB, tA),
			},
			flash:     flashV3,
			wantMixed: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Cycle{Edges: tc.edges}
			got3 := qualifyForV3Mini(c, tc.flash)
			got4 := qualifyForV4Mini(c, tc.flash)
			gotM := qualifyForMixedV3V4(c, tc.flash)

			useV3 := got3
			useV4 := !useV3 && got4
			useMx := !useV3 && !useV4 && gotM

			if useV3 != tc.wantV3 {
				t.Errorf("useV3Mini: got %v want %v (raw qualify=%v)", useV3, tc.wantV3, got3)
			}
			if useV4 != tc.wantV4 {
				t.Errorf("useV4Mini: got %v want %v (raw qualify=%v)", useV4, tc.wantV4, got4)
			}
			if useMx != tc.wantMixed {
				t.Errorf("useMixedV3V4: got %v want %v (raw qualify=%v)", useMx, tc.wantMixed, gotM)
			}

			n := 0
			if useV3 {
				n++
			}
			if useV4 {
				n++
			}
			if useMx {
				n++
			}
			if n > 1 {
				t.Errorf("multiple minis selected simultaneously: v3=%v v4=%v mixed=%v", useV3, useV4, useMx)
			}
		})
	}
}

func TestCycleShapeGatesMatchQualify(t *testing.T) {
	tA := NewToken("0x0000000000000000000000000000000000000001", "A", 18)
	tB := NewToken("0x0000000000000000000000000000000000000002", "B", 18)
	mk := func(dex DEXType) Edge {
		return Edge{Pool: &Pool{DEX: dex, Token0: tA, Token1: tB}, TokenIn: tA, TokenOut: tB}
	}

	check := func(name string, dexes []DEXType, wantV3Shape, wantV4Shape, wantMixShape bool) {
		edges := make([]Edge, len(dexes))
		for i, d := range dexes {
			edges[i] = mk(d)
		}
		c := Cycle{Edges: edges}
		if got := cycleIsV3MiniShape(c); got != wantV3Shape {
			t.Errorf("%s: cycleIsV3MiniShape got %v want %v", name, got, wantV3Shape)
		}
		if got := cycleIsV4MiniShape(c); got != wantV4Shape {
			t.Errorf("%s: cycleIsV4MiniShape got %v want %v", name, got, wantV4Shape)
		}
		if got := cycleIsMixedV3V4Shape(c); got != wantMixShape {
			t.Errorf("%s: cycleIsMixedV3V4Shape got %v want %v", name, got, wantMixShape)
		}
	}

	check("pureV3", []DEXType{DEXUniswapV3, DEXSushiSwapV3}, true, false, false)
	check("pureV4", []DEXType{DEXUniswapV4, DEXUniswapV4}, false, true, false)
	check("mixed", []DEXType{DEXUniswapV4, DEXUniswapV3}, false, false, true)
	check("withV2", []DEXType{DEXUniswapV3, DEXUniswapV2}, false, false, false)
}
