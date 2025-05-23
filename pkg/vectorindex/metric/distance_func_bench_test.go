// Copyright 2024 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package metric

import (
	"math/rand"
	"testing"
)

/*
Benchmark_L2Distance/L2_Distance-10         	                    1570082	      1014 ns/op
Benchmark_L2Distance/Normalize_L2-10        	                    1277733	      1064 ns/op
Benchmark_L2Distance/L2_Distance(v1,_NormalizeL2)-10         	     589376	      1883 ns/op
*/
func Benchmark_L2Distance(b *testing.B) {
	dim := 128

	b.Run("L2 Distance", func(b *testing.B) {
		v1, v2 := randomVectors(b.N, dim), randomVectors(b.N, dim)
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			_, _ = L2Distance[float64](v1[i], v2[i])
		}
	})

	b.Run("Normalize L2", func(b *testing.B) {
		v1 := randomVectors(b.N, dim)
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			res := make([]float64, dim)
			_ = NormalizeL2[float64](v1[i], res)
		}
	})

	b.Run("L2 Distance(v1, NormalizeL2)", func(b *testing.B) {
		v1, v2 := randomVectors(b.N, dim), randomVectors(b.N, dim)
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			res := make([]float64, dim)
			_ = NormalizeL2[float64](v2[i], res)
			_, _ = L2Distance[float64](v1[i], res)
		}
	})

}

func randomVectors(size, dim int) [][]float64 {
	vectors := make([][]float64, size)
	for i := range vectors {
		for j := 0; j < dim; j++ {
			vectors[i] = append(vectors[i], rand.Float64())
		}
	}
	return vectors
}
