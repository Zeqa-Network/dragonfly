package block

import (
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/internal/block_internal"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/internal/world_internal"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/world"
	"math"
	"sync"
)

// Liquid represents a block that can be moved through and which can flow in the world after placement. There
// are two liquids in vanilla, which are lava and water.
type Liquid interface {
	world.Block
	// LiquidDepth returns the current depth of the liquid.
	LiquidDepth() int
	// SpreadDecay returns the amount of depth that is subtracted from the liquid's depth when it spreads to
	// a next block.
	SpreadDecay() int
	// WithDepth returns the liquid with the depth passed.
	WithDepth(depth int, falling bool) Liquid
	// LiquidFalling checks if the liquid is currently considered falling down.
	LiquidFalling() bool
	// LiquidType returns a string unique for the liquid, used to check if two liquids are considered to be
	// of the same type.
	LiquidType() string
	// Harden checks if the block should harden when looking at the surrounding blocks and sets the position
	// to the hardened block when adequate. If the block was hardened, the method returns true.
	Harden(pos world.BlockPos, w *world.World, flownIntoBy *world.BlockPos) bool
}

// LiquidRemovable represents a block that may be removed by a liquid flowing into it. When this happens, the
// block's drops are dropped at the position if HasLiquidDrops returns true.
type LiquidRemovable interface {
	HasLiquidDrops() bool
}

// tickLiquid ticks the liquid block passed at a specific position in the world. Depending on the surroundings
// and the liquid block, the liquid will either spread or decrease in depth. Additionally, the liquid might
// be turned into a solid block if a different liquid is next to it.
func tickLiquid(b Liquid, pos world.BlockPos, w *world.World) {
	if !source(b) && !sourceAround(b, pos, w) {
		if b.LiquidDepth()-4 <= 0 {
			w.PlaceBlock(pos, Air{})
			return
		}
		w.PlaceBlock(pos, b.WithDepth(b.LiquidDepth()-2*b.SpreadDecay(), false))
		return
	}

	canFlowBelow := canFlowInto(b, w, pos.Add(world.BlockPos{0, -1}), false)
	if b.LiquidFalling() && !canFlowBelow {
		b = b.WithDepth(8, true)
	} else if canFlowBelow {
		flowInto(b.WithDepth(8, true), pos, pos.Add(world.BlockPos{0, -1}), w, true)
	}

	depth, decay := b.LiquidDepth(), b.SpreadDecay()
	if depth <= decay {
		// Current depth is smaller than the decay, so spreading will result in nothing.
		return
	}
	if source(b) || !canFlowBelow {
		paths := calculateLiquidPaths(b, pos, w)
		if len(paths) == 0 {
			spreadOutwards(b, pos, w)
			return
		}

		smallestLen := len(paths[0])
		for _, path := range paths {
			if len(path) <= smallestLen {
				flowInto(b, pos, path[0], w, false)
			}
		}
	}
}

// source checks if a liquid is a source block.
func source(b Liquid) bool {
	return b.LiquidDepth() == 8 && !b.LiquidFalling()
}

// spreadOutwards spreads the liquid outwards into the horizontal directions.
func spreadOutwards(b Liquid, pos world.BlockPos, w *world.World) {
	pos.Neighbours(func(neighbour world.BlockPos) {
		if neighbour[1] == pos[1] {
			flowInto(b, pos, neighbour, w, false)
		}
	})
}

// sourceAround checks if there is a source in the blocks around the position passed.
func sourceAround(b Liquid, pos world.BlockPos, w *world.World) (sourcePresent bool) {
	pos.Neighbours(func(neighbour world.BlockPos) {
		if neighbour[1] == pos[1]-1 {
			// We don't care about water below this one.
			return
		}
		side, ok := w.Block(neighbour).(Liquid)
		if !ok || side.LiquidType() != b.LiquidType() {
			return
		}
		if neighbour[1] == pos[1]+1 || source(side) || side.LiquidDepth() > b.LiquidDepth() {
			sourcePresent = true
		}
	})
	return
}

// flowInto makes the liquid passed flow into the position passed in a world. If successful, the block at that
// position will be broken and the liquid with a lower depth will replace it.
func flowInto(b Liquid, src, pos world.BlockPos, w *world.World, falling bool) bool {
	newDepth := b.LiquidDepth() - b.SpreadDecay()
	if falling {
		newDepth = b.LiquidDepth()
	}
	if newDepth <= 0 && !falling {
		return false
	}
	existing := w.Block(pos)
	if existingLiquid, alsoLiquid := existing.(Liquid); alsoLiquid && existingLiquid.LiquidType() == b.LiquidType() {
		if existingLiquid.LiquidDepth() >= newDepth || existingLiquid.LiquidFalling() {
			// The existing liquid had a higher depth than the one we're propagating or it was falling
			// (basically considered full depth), so no need to continue.
			return true
		}
		w.PlaceBlock(pos, b.WithDepth(newDepth, falling))
		return true
	} else if alsoLiquid {
		existingLiquid.Harden(pos, w, &src)
		return false
	}
	removable, ok := existing.(LiquidRemovable)
	if !ok {
		// Can't flow into this block.
		return false
	}
	if _, air := existing.(Air); !air {
		w.BreakBlock(pos)
	}
	if removable.HasLiquidDrops() {
		it, ok := existing.(world.Item)
		if !ok {
			// Should never happen.
			panic("blocks removable by liquid with drops should always implement world.Item")
		}
		// TODO: Drop item entities.
		_ = it
	}
	w.PlaceBlock(pos, b.WithDepth(newDepth, falling))
	return true
}

// liquidPath represents a path to an empty lower block or a block that can be flown into by a liquid, which
// the liquid tends to flow into. All paths with the lowest length will be filled with water.
type liquidPath []world.BlockPos

// calculateLiquidPaths calculates paths in the world that the liquid passed can flow in to reach lower
// grounds, starting at the position passed.
// If none of these paths can be found, the returned slice has a length of 0.
func calculateLiquidPaths(b Liquid, pos world.BlockPos, w *world.World) []liquidPath {
	queue := liquidQueuePool.Get().(*liquidQueue)
	defer func() {
		queue.Reset()
		liquidQueuePool.Put(queue)
	}()
	queue.PushBack(liquidNode{x: pos[0], z: pos[2], depth: int8(b.LiquidDepth())})
	decay := int8(b.SpreadDecay())

	paths := make([]liquidPath, 0, 3)

	for {
		if queue.Len() == 0 {
			break
		}
		node := queue.Front()
		neighA, neighB, neighC, neighD := node.neighbours(decay * 2)
		if spreadNeighbour(b, pos, w, neighA, queue) {
			queue.shortestPath = neighA.Len()
			paths = append(paths, neighA.Path(pos))
		}
		if spreadNeighbour(b, pos, w, neighB, queue) {
			queue.shortestPath = neighB.Len()
			paths = append(paths, neighB.Path(pos))
		}
		if spreadNeighbour(b, pos, w, neighC, queue) {
			queue.shortestPath = neighC.Len()
			paths = append(paths, neighC.Path(pos))
		}
		if spreadNeighbour(b, pos, w, neighD, queue) {
			queue.shortestPath = neighD.Len()
			paths = append(paths, neighD.Path(pos))
		}
	}
	return paths
}

// spreadNeighbour attempts to spread a path node into the neighbour passed. Note that this does not spread
// the liquid, it only spreads the node used to calculate flow paths.
func spreadNeighbour(b Liquid, src world.BlockPos, w *world.World, node liquidNode, queue *liquidQueue) bool {
	if node.depth+3 <= 0 {
		// Depth has reached zero or below, can't spread any further.
		return false
	}
	if node.Len() > queue.shortestPath {
		// This path is longer than any existing path, so don't spread any further.
		return false
	}
	pos := world.BlockPos{node.x, src[1], node.z}
	if !canFlowInto(b, w, pos, true) {
		// Can't flow into this block, can't spread any further.
		return false
	}
	pos[1]--
	if canFlowInto(b, w, pos, false) {
		return true
	}
	queue.PushBack(node)
	return false
}

// canFlowInto checks if a liquid can flow into the block present in the world at a specific block position.
func canFlowInto(b Liquid, w *world.World, pos world.BlockPos, sideways bool) bool {
	rid := block_internal.World_runtimeID(w, pos)
	if rid == 0 {
		return true
	}
	_, ok := world_internal.LiquidRemovable[rid]
	if ok && sideways {
		if liq, ok := w.Block(pos).(Liquid); ok && (liq.LiquidDepth() == 8 || liq.LiquidType() != b.LiquidType()) {
			return false
		}
	}
	return ok
}

// liquidNode represents a position that is part of a flow path for a liquid.
type liquidNode struct {
	x, z     int
	depth    int8
	previous *liquidNode
}

// neighbours returns the four horizontal neighbours of the node with decreased depth.
func (node liquidNode) neighbours(decay int8) (a, b, c, d liquidNode) {
	return liquidNode{x: node.x - 1, z: node.z, depth: node.depth - decay, previous: &node},
		liquidNode{x: node.x + 1, z: node.z, depth: node.depth - decay, previous: &node},
		liquidNode{x: node.x, z: node.z - 1, depth: node.depth - decay, previous: &node},
		liquidNode{x: node.x, z: node.z + 1, depth: node.depth - decay, previous: &node}
}

// Len returns the length of the path created by the node.
func (node liquidNode) Len() int {
	i := 1
	for {
		if node.previous == nil {
			return i - 1
		}
		//noinspection GoAssignmentToReceiver
		node = *node.previous
		i++
	}
}

// Path converts the liquid node into a path.
func (node liquidNode) Path(src world.BlockPos) liquidPath {
	l := node.Len()
	path := make(liquidPath, l)
	i := l - 1
	for {
		if node.previous == nil {
			return path
		}
		path[i] = world.BlockPos{node.x, src[1], node.z}

		//noinspection GoAssignmentToReceiver
		node = *node.previous
		i--
	}
}

// liquidQueuePool is use to re-use liquid node queues.
var liquidQueuePool = sync.Pool{
	New: func() interface{} {
		return &liquidQueue{
			nodes:        make([]liquidNode, 0, 64),
			shortestPath: math.MaxInt8,
		}
	},
}

// liquidQueue represents a queue that may be used to push nodes into and take them out of it.
type liquidQueue struct {
	nodes        []liquidNode
	i            int
	shortestPath int
}

func (q *liquidQueue) PushBack(node liquidNode) {
	q.nodes = append(q.nodes, node)
}

func (q *liquidQueue) Front() liquidNode {
	v := q.nodes[q.i]
	q.i++
	return v
}

func (q *liquidQueue) Len() int {
	return len(q.nodes) - q.i
}

func (q *liquidQueue) Reset() {
	q.nodes = q.nodes[:0]
	q.i = 0
	q.shortestPath = math.MaxInt8
}