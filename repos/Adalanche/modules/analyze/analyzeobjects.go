package analyze

import (
	"sort"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/lkarlslund/adalanche/modules/engine"
	"github.com/lkarlslund/adalanche/modules/graph"
	"github.com/lkarlslund/adalanche/modules/query"
	"github.com/lkarlslund/adalanche/modules/ui"
	"github.com/lkarlslund/adalanche/modules/util"
	"github.com/lkarlslund/adalanche/modules/windowssecurity"
)

var SortBy engine.Attribute = engine.NonExistingAttribute

var EdgeMemberOfGroup = engine.NewEdge("MemberOfGroup") // Get rid of this

func NewAnalyzeObjectsOptions() AnalyzeOptions {
	return AnalyzeOptions{
		EdgesFirst:                engine.AllEdgesBitmap,
		EdgesMiddle:               engine.AllEdgesBitmap,
		EdgesLast:                 engine.AllEdgesBitmap,
		Direction:                 engine.In,
		MaxDepth:                  -1,
		MaxOutgoingConnections:    -1,
		MinEdgeProbability:        0,
		MinAccumulatedProbability: 0,
		PruneIslands:              false,
		DontExpandAUEO:            true,
	}
}

type AnalyzeOptions struct {
	Name                      string
	FilterFirst               query.NodeFilter
	FilterMiddle              query.NodeFilter
	FilterLast                query.NodeFilter
	ObjectTypesFirst          map[engine.ObjectType]struct{}
	ObjectTypesMiddle         map[engine.ObjectType]struct{}
	ObjectTypesLast           map[engine.ObjectType]struct{}
	EdgesFirst                engine.EdgeBitmap
	EdgesMiddle               engine.EdgeBitmap
	EdgesLast                 engine.EdgeBitmap
	MaxDepth                  int
	MaxOutgoingConnections    int
	Direction                 engine.EdgeDirection
	Backlinks                 int // Backlink depth
	MinEdgeProbability        engine.Probability
	MinAccumulatedProbability engine.Probability
	PruneIslands              bool
	DontExpandAUEO            bool
	AllDetails                bool
	NodeLimit                 int
}

func ParseQueryFromPOST(ctx *gin.Context, objects *engine.Objects) (*AnalyzeOptions, error) {
	qd, err := ParseQueryDefinitionFromPOST(ctx)
	if err != nil {
		return nil, err
	}

	aoo, err := qd.AnalysisOptions(objects)
	if err != nil {
		return nil, err
	}

	// Grab other settings not processed by the query parser
	params := make(map[string]any)
	err = ctx.ShouldBindBodyWithJSON(&params)
	if err != nil {
		return nil, err
	}

	if alld, ok := params["alldetails"].(string); ok {
		aoo.AllDetails, _ = util.ParseBool(alld)
	}
	if nodelimit, ok := params["nodelimit"].(string); ok {
		aoo.NodeLimit, _ = strconv.Atoi(nodelimit)
	}

	// tricky tricky - if we get a call with the expanddn set, then we handle things .... differently :-)
	// if expanddn := params["expanddn"]; expanddn != "" {
	// 	qd.QueryStart = `(distinguishedName=` + expanddn + `)`
	// 	qd.MaxOutgoingConnections = 0
	// 	qd.MaxDepth = 1
	// 	aoo.NodeLimit = 1000
	// }

	// Default to all edges if none are specified
	if aoo.EdgesFirst.Count() == 0 && aoo.EdgesMiddle.Count() == 0 && aoo.EdgesLast.Count() == 0 {
		// Spread the choices to FME
		aoo.EdgesFirst = engine.AllEdgesBitmap
		aoo.EdgesMiddle = engine.AllEdgesBitmap
		aoo.EdgesLast = engine.AllEdgesBitmap
	}

	// Parse object types into map of objectType
	aoo.ObjectTypesFirst, err = ParseObjectTypeStrings(qd.ObjectTypesFirst)
	if err != nil {
		return nil, err
	}
	aoo.ObjectTypesMiddle, err = ParseObjectTypeStrings(qd.ObjectTypesMiddle)
	if err != nil {
		return nil, err
	}
	aoo.ObjectTypesLast, err = ParseObjectTypeStrings(qd.ObjectTypesLast)
	if err != nil {
		return nil, err
	}

	return &aoo, nil
}

type GraphNode struct {
	CanExpand              int
	processRound           int
	accumulatedprobability float32 // 0-1
}

type PostProcessorFunc func(pg graph.Graph[*engine.Object, engine.EdgeBitmap]) graph.Graph[*engine.Object, engine.EdgeBitmap]

var PostProcessors []PostProcessorFunc

// type AnalysisNode struct {
// 	*engine.Object
// 	engine.DynamicFields
// }

type AnalysisResults struct {
	Graph   graph.Graph[*engine.Object, engine.EdgeBitmap]
	Removed int
}

func Analyze(opts AnalyzeOptions, objects *engine.Objects) AnalysisResults {

	pg := graph.NewGraph[*engine.Object, engine.EdgeBitmap]()
	extrainfo := make(map[*engine.Object]*GraphNode)

	// Convert to our working graph
	currentRound := 1
	query.Execute(opts.FilterFirst, objects).Iterate(func(o *engine.Object) bool {
		pg.SetNodeData(o, "target", true)

		for o := range pg.Nodes() {
			if ei, found := extrainfo[o]; !found || ei.processRound == 0 {
				extrainfo[o] = (&GraphNode{
					processRound:           currentRound,
					accumulatedprobability: 1,
				})
			}
		}

		return true
	})

	// Methods and ObjectTypes allowed
	detectedges := opts.EdgesFirst
	detectobjecttypes := opts.ObjectTypesFirst

	pb := ui.ProgressBar("Analyzing graph", int64(opts.MaxDepth))
	for opts.MaxDepth >= currentRound || opts.MaxDepth == -1 {
		pb.Add(1)
		if currentRound == 2 {
			detectedges = opts.EdgesMiddle
			detectobjecttypes = nil
			if len(opts.ObjectTypesMiddle) > 0 {
				detectobjecttypes = opts.ObjectTypesMiddle
			}
		}

		ui.Debug().Msgf("Starting round %v with %v total objects and %v connections", currentRound, pg.Order(), pg.Size())

		nodesatstartofround := pg.Order()

		for currentobject := range pg.Nodes() {
			// All nodes need to be processed in the next round
			ei := extrainfo[currentobject]

			if ei.processRound != currentRound /* shouldn't be processed this round */ {
				continue
			}

			newconnectionsmap := make(map[graph.NodePair[*engine.Object]]engine.EdgeBitmap) // Pwn Connection between objects

			if opts.Direction == engine.In && opts.DontExpandAUEO && (currentobject.SID() == windowssecurity.EveryoneSID || currentobject.SID() == windowssecurity.AuthenticatedUsersSID) {
				// Don't expand Authenticated Users or Everyone
				continue
			}

			// Iterate over ever edges
			currentobject.Edges(opts.Direction).Range(func(nextobject *engine.Object, eb engine.EdgeBitmap) bool {
				// If this is not a chosen edge, skip it
				detectededges := eb.Intersect(detectedges)

				if detectededges.IsBlank() {
					// Nothing useful or just a deny ACL, skip it
					return true // continue
				}

				if detectobjecttypes != nil {
					if _, found := detectobjecttypes[nextobject.Type()]; !found {
						// We're filtering on types, and it's not wanted
						return true //continue
					}
				}

				// Edge probability
				var maxprobability engine.Probability
				if opts.Direction == engine.In {
					maxprobability = detectededges.MaxProbability(nextobject, currentobject)
				} else {
					maxprobability = detectededges.MaxProbability(currentobject, nextobject)
				}
				if maxprobability < engine.Probability(opts.MinEdgeProbability) {
					// Too unlikeliy, so we skip it
					return true // continue
				}

				// Accumulated node probability
				accumulatedprobability := ei.accumulatedprobability * float32(maxprobability) / 100
				if accumulatedprobability < float32(opts.MinAccumulatedProbability)/100 {
					// Too unlikeliy, so we skip it
					return true // continue
				}

				// If we allow backlinks, all pwns are mapped, no matter who is the victim
				// Targets are allowed to pwn each other as a way to reach the goal of pwning all of them
				// If pwner is already processed, we don't care what it can pwn someone more far away from targets
				// If pwner is our attacker, we always want to know what it can do
				found := pg.HasNode(nextobject) // It could JUST have been added to the graph by another node in current processing round though

				// SKIP THIS IF
				if
				// We're not including backlinks
				found &&
					// This is not the first round
					currentRound > 1 &&
					// It was found in an earlier round
					extrainfo[nextobject] != nil && extrainfo[nextobject].processRound+opts.Backlinks <= currentRound &&
					// If SIDs match between objects, it's a cross forest/domain link and we want to see it
					(currentobject.SID().IsNull() || nextobject.SID().IsNull() || currentobject.SID().Component(2) != 21 || currentobject.SID() != nextobject.SID()) {
					// skip it
					return true // continue
				}

				if opts.FilterMiddle != nil && !opts.FilterMiddle.Evaluate(nextobject) {
					// skip unwanted middle objects
					return true // continue
				}

				if opts.Direction == engine.In {
					newconnectionsmap[graph.NodePair[*engine.Object]{
						Source: nextobject,
						Target: currentobject}] = detectededges
				} else {
					newconnectionsmap[graph.NodePair[*engine.Object]{
						Source: currentobject,
						Target: nextobject}] = detectededges
				}

				if currentRound != 1 || extrainfo[nextobject] == nil {
					// First round is special, as we process the targets
					// All the other rounds, we can assume that nextobjects are new in the graph
					extrainfo[nextobject] = &GraphNode{
						processRound:           currentRound + 1,
						accumulatedprobability: ei.accumulatedprobability * float32(maxprobability) / 100,
					}
				}

				return true
			})

			if opts.MaxOutgoingConnections == -1 || len(newconnectionsmap) < opts.MaxOutgoingConnections {
				for pwnpair, detectedmethods := range newconnectionsmap {
					pg.AddEdge(pwnpair.Source, pwnpair.Target, detectedmethods)
				}
				// Add pwn target to graph for processing
			} else {
				ui.Debug().Msgf("Outgoing expansion limit hit %v for object %v, there was %v connections", opts.MaxOutgoingConnections, currentobject.Label(), len(newconnectionsmap))
				var added int
				var groupcount int
				for _, detectedmethods := range newconnectionsmap {
					// We assume the number of groups are limited and add them anyway
					if detectedmethods.IsSet(EdgeMemberOfGroup) {
						groupcount++
					}
				}

				if groupcount < opts.MaxOutgoingConnections {
					// Add the groups, but not the rest
					for pwnpair, detectedmethods := range newconnectionsmap {
						// We assume the number of groups are limited and add them anyway
						if detectedmethods.IsSet(EdgeMemberOfGroup) {
							pg.AddEdge(pwnpair.Source, pwnpair.Target, detectedmethods)
							delete(newconnectionsmap, pwnpair)
							added++
						}
					}
					ui.Debug().Msgf("Expansion limit compromise - added %v groups as they fit under the expansion limit %v", added, opts.MaxOutgoingConnections)
				}

				// Add some more to expansion limit hit objects if we know how
				if SortBy != engine.NonExistingAttribute {
					var additionaladded int

					// Find the most important ones that are not groups
					var notadded []graph.GraphNodePairEdge[*engine.Object, engine.EdgeBitmap]
					for pwnpair, detectedmethods := range newconnectionsmap {
						notadded = append(notadded, graph.GraphNodePairEdge[*engine.Object, engine.EdgeBitmap]{
							Source: pwnpair.Source,
							Target: pwnpair.Target,
							Edge:   detectedmethods,
						})
					}

					if SortBy != engine.NonExistingAttribute {
						sort.Slice(notadded, func(i, j int) bool {
							if opts.Direction == engine.In {
								iv, _ := notadded[i].Source.AttrInt(SortBy)
								jv, _ := notadded[j].Source.AttrInt(SortBy)
								return iv > jv
							}
							iv, _ := notadded[i].Target.AttrInt(SortBy)
							jv, _ := notadded[j].Target.AttrInt(SortBy)
							return iv > jv
						})
					}

					// Add up to limit
					for i := 0; i+added < opts.MaxOutgoingConnections && i < len(notadded); i++ {
						pg.AddEdge(notadded[i].Source, notadded[i].Target, notadded[i].Edge)
						additionaladded++
					}

					ui.Debug().Msgf("Added additionally %v prioritized objects", additionaladded)
					added += additionaladded
				}

				ei.CanExpand = len(newconnectionsmap) - added
			}
		}
		ui.Debug().Msgf("Processing round %v yielded %v new objects", currentRound, pg.Order()-nodesatstartofround)

		if nodesatstartofround == pg.Order() {
			// Nothing was added, we're done
			break
		}

		currentRound++
	}
	pb.Finish()

	ui.Debug().Msgf("Analysis result total %v objects", pg.Order())

	if len(extrainfo) != pg.Order() {
		ui.Warn().Msgf("Not all nodes were processed. Expected %v, processed %v", pg.Order(), len(extrainfo))
	}

	pb = ui.ProgressBar("Removing filtered nodes", int64(pg.Order()))

	// Remove outer end nodes that are invalid
	detectobjecttypes = nil
	if len(opts.ObjectTypesLast) > 0 {
		detectobjecttypes = opts.ObjectTypesLast
	}

	// Keep removing stuff while it makes sense
	for {
		var removed int

		// This map contains all the nodes that is pointed by someone else. If you're in this map you're not an outer node
		var outernodes []*engine.Object
		if opts.Direction == engine.In {
			outernodes = pg.StartingNodes()
		} else {
			outernodes = pg.EndingNodes()
		}
		outernodemap := make(map[*engine.Object]struct{})

		for _, outernode := range outernodes {
			outernodemap[outernode] = struct{}{}
		}

		pg.IterateEdges(func(source, target *engine.Object, endedge engine.EdgeBitmap) bool {
			var endnode *engine.Object
			if opts.Direction == engine.In {
				endnode = source
			} else {
				endnode = target
			}
			if _, found := outernodemap[endnode]; found {
				// Outer node
				if opts.EdgesLast.Intersect(endedge).Count() == 0 {
					// No matches on LastMethods
					pg.DeleteNode(endnode)
					pb.Add(1)
					removed++
					return true
				}
				if detectobjecttypes != nil {
					if _, found := detectobjecttypes[endnode.Type()]; !found {
						// No matches on LastMethods
						ui.Debug().Msgf("Removing %v of type %v because not in list of %v", endnode.DN(), endnode.Type(), detectobjecttypes)
						pg.DeleteNode(endnode)
						pb.Add(1)
						removed++
						return true
					}
				}
				if opts.FilterLast != nil && !opts.FilterLast.Evaluate(endnode) {
					pg.DeleteNode(endnode)
					pb.Add(1)
					removed++
					return true
				}
			}
			return true
		})

		if removed == 0 {
			break
		}

		ui.Debug().Msgf("Post graph object filtering processing round removed %v nodes", removed)
	}
	pb.Finish()

	var outernodes []*engine.Object
	if opts.Direction == engine.In {
		outernodes = pg.StartingNodes()
	} else {
		outernodes = pg.EndingNodes()
	}
	for _, outernode := range outernodes {
		pg.SetNodeData(outernode, "source", true)
	}

	ui.Debug().Msgf("After filtering we have %v objects", pg.Order())

	totalnodes := pg.Order()
	toomanynodes := pg.Order() - opts.NodeLimit
	if opts.NodeLimit > 0 && toomanynodes > 0 {
		// Prune nodes until we dont have too many
		lefttoremove := toomanynodes
		pb = ui.ProgressBar("Removing random excessive outer nodes", int64(lefttoremove))

		for lefttoremove > 0 {
			// This map contains all the nodes that point to someone else. If you're in this map you're not an outer node
			var removedthisround, maxround int

			pointedtobysomeone := make(map[*engine.Object]struct{})
			var outernodes []*engine.Object
			if opts.Direction == engine.In {
				outernodes = pg.StartingNodes()
			} else {
				outernodes = pg.EndingNodes()
			}
			for _, outernode := range outernodes {
				pointedtobysomeone[outernode] = struct{}{}
				if maxround < extrainfo[outernode].processRound {
					maxround = extrainfo[outernode].processRound
				}
			}

			for _, outernode := range outernodes {
				if extrainfo[outernode].processRound == maxround {
					pg.DeleteNode(outernode)
					pb.Add(1)
					lefttoremove--
					removedthisround++
				}
				if lefttoremove == 0 {
					break
				}
			}
			if removedthisround == 0 && lefttoremove > 0 {
				ui.Warn().Msgf("Could not find any outer nodes to remove, still should remove %v nodes", lefttoremove)
				break
			}
		}
	}

	pb.Finish()

	// PruneIslands
	var prunedislands int
	if opts.PruneIslands {
		// Find island nodes
		for _, islandnode := range pg.Islands() {
			pg.DeleteNode(islandnode)
			prunedislands++
		}
	}
	if prunedislands > 0 {
		ui.Debug().Msgf("Pruning islands removed %v nodes", prunedislands)
		ui.Debug().Msgf("After pruning we have %v objects", pg.Order())

	}

	// Mark outer nodes for graph visualization
	if opts.Direction == engine.In {
		outernodes = pg.StartingNodes()
	} else {
		outernodes = pg.EndingNodes()
	}
	for _, node := range outernodes {
		pg.SetNodeData(node, "source", true)
	}

	ui.Info().Msgf("Graph query resulted in %v nodes", pg.Order())

	pg.Nodes() // Trigger cleanup, important otherwise they get readded below
	for eo, ei := range extrainfo {
		if pg.HasNode(eo) && ei.CanExpand > 0 {
			pg.SetNodeData(eo, "canexpand", ei.CanExpand)
		}
	}

	ui.Debug().Msgf("Final analysis node count is %v objects", pg.Order())

	return AnalysisResults{
		Graph:   pg,
		Removed: totalnodes - pg.Order(),
	}
}
