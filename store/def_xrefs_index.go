// Update updates the defRefsIndex with new references. All existing

import (
	"sourcegraph.com/sourcegraph/srclib/graph"
	"sourcegraph.com/sourcegraph/srclib/store/phtable"
)

// defRefsIndex makes it fast to determine get all the xrefs to a def
type defXRefsIndex struct {
	phtable *phtable.CHD
	ready   bool
}

// references from the same source units of refs are deleted and
// replaced with refs.
func (x *defXRefsIndex) Build(refs []*graph.Ref, fbr fileByteRanges, ofs byteOffsets) error {
	return nil
}

// references from the same source units of refs are deleted and
// replaced with refs.
func (x *defXRefsIndex) Update(refs []*graph.Ref, fbr fileByteRanges, ofs byteOffsets) error {
	return nil
}

