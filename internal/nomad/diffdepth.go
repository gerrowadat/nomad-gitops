package nomad

// MaxPlanDiffObjectDepth caps how deep nomad-gitops recurses into a Nomad
// plan-diff's Objects tree (classification, redaction; internal/server/render.go
// mirrors this cap for text rendering). A legitimate job spec never nests
// anywhere near this deep — real ObjectDiff trees bottom out within a handful
// of levels (job/group/task/config). Depth beyond this cap can only come from
// a deliberately crafted HCL job spec, and traversal stops there rather than
// recursing without bound: unbounded recursion risks a stack-overflow crash,
// which is not a recoverable panic and would take down the whole process, not
// just the one job's check.
const MaxPlanDiffObjectDepth = 200
