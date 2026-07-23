// Package selector implements matching for the v1alpha1.Selector type
// (identity attributes + labels + expression operators).
//
// It is kept independent of any specific consumer so it can be reused as a
// shared filtering vocabulary. Callers adapt their own objects into a
// Matchable{Identity, Labels} and pass it to Matches.
package selector
