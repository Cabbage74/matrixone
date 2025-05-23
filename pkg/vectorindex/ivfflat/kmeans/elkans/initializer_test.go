// Copyright 2023 Matrix Origin
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

package elkans

import (
	"reflect"
	"testing"

	"github.com/matrixorigin/matrixone/pkg/vectorindex/metric"
	"github.com/stretchr/testify/require"
)

func TestRandom_InitCentroids(t *testing.T) {
	type args struct {
		vectors [][]float64
		k       int
	}
	tests := []struct {
		name          string
		args          args
		wantCentroids [][]float64
	}{
		{
			name: "TestRandom_InitCentroids",
			args: args{
				vectors: [][]float64{
					{1, 2, 3, 4},
					{1, 2, 4, 5},
					{1, 2, 4, 5},
					{1, 2, 3, 4},
					{1, 2, 4, 5},
					{1, 2, 4, 5},
					{10, 2, 4, 5},
					{10, 3, 4, 5},
					{10, 5, 4, 5},
					{10, 2, 4, 5},
					{10, 3, 4, 5},
					{10, 5, 4, 5},
				},
				k: 2,
			},
			wantCentroids: [][]float64{
				// NOTE: values of random initialization need not be farther apart, it is random.
				// NOTE: we get the same random values in the test case because we are using a constant seed value.
				{1, 2, 4, 5},
				{1, 2, 3, 4},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRandomInitializer()
			_gotCentroids, err := r.InitCentroids(tt.args.vectors, tt.args.k)
			require.Nil(t, err)
			gotCentroids, ok := _gotCentroids.([][]float64)
			require.True(t, ok)
			if !reflect.DeepEqual(gotCentroids, tt.wantCentroids) {
				t.Errorf("InitCentroids() = %v, want %v", gotCentroids, tt.wantCentroids)
			}

		})
	}
}

func TestKMeansPlusPlus_InitCentroids(t *testing.T) {
	type args struct {
		vectors [][]float64
		k       int
	}
	tests := []struct {
		name          string
		args          args
		wantCentroids [][]float64
	}{
		{
			name: "TestKMeansPlusPlus_InitCentroids",
			args: args{
				vectors: [][]float64{
					{1, 2, 3, 4},
					{1, 2, 4, 5},
					{1, 2, 4, 5},
					{1, 2, 3, 4},
					{1, 2, 4, 5},
					{1, 2, 4, 5},
					{10, 2, 4, 5},
					{10, 3, 4, 5},
					{10, 5, 4, 5},
					{10, 2, 4, 5},
					{10, 3, 4, 5},
					{10, 5, 4, 5},
				},
				k: 2,
			},
			// Kmeans++ picked the relatively farthest points as the initial centroids
			wantCentroids: [][]float64{
				{1, 2, 4, 5},
				{10, 5, 4, 5},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewKMeansPlusPlusInitializer[float64](metric.L2Distance[float64])
			_gotCentroids, err := r.InitCentroids(tt.args.vectors, tt.args.k)
			require.Nil(t, err)
			gotCentroids := _gotCentroids.([][]float64)
			if !reflect.DeepEqual(gotCentroids, tt.wantCentroids) {
				t.Errorf("InitCentroids() = %v, want %v", gotCentroids, tt.wantCentroids)
			}
		})
	}
}

/*
date : 2023-11-20
goos: darwin
goarch: arm64
cpu: Apple M2 Pro
rows: 10_000
dims: 1024
k : 10
Benchmark_InitCentroids/RANDOM-10         		108	        10574740 ns/op
Benchmark_InitCentroids/KMEANS++-10       	      1		  1081363458 ns/op
*/
func Benchmark_InitCentroids(b *testing.B) {
	rowCnt := 10_000
	dims := 1024
	k := 10

	data := make([][]float64, rowCnt)
	populateRandData(rowCnt, dims, data)

	random := NewRandomInitializer()
	kmeanspp := NewKMeansPlusPlusInitializer[float64](metric.L2Distance[float64])

	b.Run("RANDOM", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := random.InitCentroids(data, k)
			require.Nil(b, err)
		}
	})

	b.Run("KMEANS++", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := kmeanspp.InitCentroids(data, k)
			require.Nil(b, err)
		}
	})
}
