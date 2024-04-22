// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package vm

import (
	"context"
	"errors"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"

	lru "github.com/hashicorp/golang-lru"
)

var (
	opcodeCommitInterruptCounter = metrics.NewRegisteredCounter("worker/opcodeCommitInterrupt", nil)
	ErrInterrupt                 = errors.New("EVM execution interrupted")
	ErrNoCache                   = errors.New("no tx cache found")
	ErrNoCurrentTx               = errors.New("no current tx found in interruptCtx")
)

const (
	// These are keys for the interruptCtx
	InterruptCtxDelayKey       = "delay"
	InterruptCtxOpcodeDelayKey = "opcodeDelay"

	// InterruptedTxCacheSize is size of lru cache for interrupted txs
	InterruptedTxCacheSize = 90000
)

// Config are the configuration options for the Interpreter
type Config struct {
	Tracer                  EVMLogger // Opcode logger
	NoBaseFee               bool      // Forces the EIP-1559 baseFee to 0 (needed for 0 price calls)
	EnablePreimageRecording bool      // Enables recording of SHA3/keccak preimages
	ExtraEips               []int     // Additional EIPS that are to be enabled
}

// ScopeContext contains the things that are per-call, such as stack and memory,
// but not transients like pc and gas
type ScopeContext struct {
	Memory     *Memory
	Stack      *Stack
	Contract   *Contract
	Authorized *common.Address
}

// EVMInterpreter represents an EVM interpreter
type EVMInterpreter struct {
	evm   *EVM
	table *JumpTable

	hasher    crypto.KeccakState // Keccak256 hasher instance shared across opcodes
	hasherBuf common.Hash        // Keccak256 hasher result array shared across opcodes

	readOnly   bool   // Whether to throw on stateful modifications
	returnData []byte // Last CALL's return data for subsequent reuse
}

// TxCache is a wrapper of lru.cache for caching transactions that get interrupted
type TxCache struct {
	Cache *lru.Cache
}

type txCacheKey struct{}
type InterruptedTxContext_currenttxKey struct{}

// SetCurrentTxOnContext sets the current tx on the context
func SetCurrentTxOnContext(ctx context.Context, txHash common.Hash) context.Context {
	return context.WithValue(ctx, InterruptedTxContext_currenttxKey{}, txHash)
}

// GetCurrentTxFromContext gets the current tx from the context
func GetCurrentTxFromContext(ctx context.Context) (common.Hash, error) {
	val := ctx.Value(InterruptedTxContext_currenttxKey{})
	if val == nil {
		return common.Hash{}, ErrNoCurrentTx
	}

	c, ok := val.(common.Hash)
	if !ok {
		return common.Hash{}, ErrNoCurrentTx
	}

	return c, nil
}

// GetCache returns the txCache from the context
func GetCache(ctx context.Context) (*TxCache, error) {
	val := ctx.Value(txCacheKey{})
	if val == nil {
		return nil, ErrNoCache
	}

	c, ok := val.(*TxCache)
	if !ok {
		return nil, ErrNoCache
	}

	return c, nil
}

// PutCache puts the txCache into the context
func PutCache(ctx context.Context, cache *TxCache) context.Context {
	return context.WithValue(ctx, txCacheKey{}, cache)
}

// NewEVMInterpreter returns a new instance of the Interpreter.
func NewEVMInterpreter(evm *EVM) *EVMInterpreter {
	// If jump table was not initialised we set the default one.
	var table *JumpTable

	switch {
	case evm.chainRules.IsPrague:
		table = &pragueInstructionSet
	case evm.chainRules.IsCancun:
		table = &cancunInstructionSet
	case evm.chainRules.IsShanghai:
		table = &shanghaiInstructionSet
	case evm.chainRules.IsMerge:
		table = &mergeInstructionSet
	case evm.chainRules.IsLondon:
		table = &londonInstructionSet
	case evm.chainRules.IsBerlin:
		table = &berlinInstructionSet
	case evm.chainRules.IsIstanbul:
		table = &istanbulInstructionSet
	case evm.chainRules.IsConstantinople:
		table = &constantinopleInstructionSet
	case evm.chainRules.IsByzantium:
		table = &byzantiumInstructionSet
	case evm.chainRules.IsEIP158:
		table = &spuriousDragonInstructionSet
	case evm.chainRules.IsEIP150:
		table = &tangerineWhistleInstructionSet
	case evm.chainRules.IsHomestead:
		table = &homesteadInstructionSet
	default:
		table = &frontierInstructionSet
	}

	var extraEips []int

	if len(evm.Config.ExtraEips) > 0 {
		// Deep-copy jumptable to prevent modification of opcodes in other tables
		table = copyJumpTable(table)
	}

	for _, eip := range evm.Config.ExtraEips {
		if err := EnableEIP(eip, table); err != nil {
			// Disable it, so caller can check if it's activated or not
			log.Error("EIP activation failed", "eip", eip, "error", err)
		} else {
			extraEips = append(extraEips, eip)
		}
	}

	evm.Config.ExtraEips = extraEips

	return &EVMInterpreter{evm: evm, table: table}
}

// PreRun is a wrapper around Run that allows for a delay to be injected before each opcode when induced by tests else it calls the legacy Run() method
func (in *EVMInterpreter) PreRun(contract *Contract, input []byte, readOnly bool, interruptCtx context.Context) (ret []byte, err error) {
	var opcodeDelay interface{}

	if interruptCtx != nil {
		if interruptCtx.Value(InterruptCtxOpcodeDelayKey) != nil {
			opcodeDelay = interruptCtx.Value(InterruptCtxOpcodeDelayKey)
		}
	}

	if opcodeDelay != nil {
		return in.RunWithDelay(contract, input, readOnly, interruptCtx, opcodeDelay.(uint))
	}

	return in.Run(contract, input, readOnly, interruptCtx)
}

// Run loops and evaluates the contract's code with the given input data and returns
// the return byte-slice and an error if one occurred.
//
// It's important to note that any errors returned by the interpreter should be
// considered a revert-and-consume-all-gas operation except for
// ErrExecutionReverted which means revert-and-keep-gas-left.
// nolint: gocognit
func (in *EVMInterpreter) Run(contract *Contract, input []byte, readOnly bool, interruptCtx context.Context) (ret []byte, err error) {
	// Increment the call depth which is restricted to 1024
	in.evm.depth++
	defer func() { in.evm.depth-- }()

	// Make sure the readOnly is only set if we aren't in readOnly yet.
	// This also makes sure that the readOnly flag isn't removed for child calls.
	if readOnly && !in.readOnly {
		in.readOnly = true
		defer func() { in.readOnly = false }()
	}

	// Reset the previous call's return data. It's unimportant to preserve the old buffer
	// as every returning call will return new data anyway.
	in.returnData = nil

	// Don't bother with the execution if there's no code.
	if len(contract.Code) == 0 {
		return nil, nil
	}

	var (
		op          OpCode        // current opcode
		mem         = NewMemory() // bound memory
		stack       = newstack()  // local stack
		callContext = &ScopeContext{
			Memory:   mem,
			Stack:    stack,
			Contract: contract,
		}
		// For optimisation reason we're using uint64 as the program counter.
		// It's theoretically possible to go above 2^64. The YP defines the PC
		// to be uint256. Practically much less so feasible.
		pc   = uint64(0) // program counter
		cost uint64
		// copies used by tracer
		pcCopy  uint64 // needed for the deferred EVMLogger
		gasCopy uint64 // for EVMLogger to log gas remaining before execution
		logged  bool   // deferred EVMLogger should ignore already logged steps
		res     []byte // result of the opcode execution function
		debug   = in.evm.Config.Tracer != nil
	)
	// Don't move this deferred function, it's placed before the capturestate-deferred method,
	// so that it gets executed _after_: the capturestate needs the stacks before
	// they are returned to the pools
	defer func() {
		returnStack(stack)
	}()

	contract.Input = input

	if debug {
		defer func() {
			if err != nil {
				if !logged {
					in.evm.Config.Tracer.CaptureState(pcCopy, op, gasCopy, cost, callContext, in.returnData, in.evm.depth, err)
				} else {
					in.evm.Config.Tracer.CaptureFault(pcCopy, op, gasCopy, cost, callContext, in.evm.depth, err)
				}
			}
		}()
	}
	// The Interpreter main run loop (contextual). This loop runs until either an
	// explicit STOP, RETURN or SELFDESTRUCT is executed, an error occurred during
	// the execution of one of the operations or until the done flag is set by the
	// parent context.
	for {
		if interruptCtx != nil {
			// case of interrupting by timeout
			select {
			case <-interruptCtx.Done():
				txHash, _ := GetCurrentTxFromContext(interruptCtx)
				interruptedTxCache, _ := GetCache(interruptCtx)

				if interruptedTxCache == nil {
					break
				}

				// if the tx is already in the cache, it means that it has been interrupted before and we will not interrupt it again
				found, _ := interruptedTxCache.Cache.ContainsOrAdd(txHash, true)
				if found {
					interruptedTxCache.Cache.Remove(txHash)
				} else {
					// if the tx is not in the cache, it means that it has not been interrupted before and we will interrupt it
					opcodeCommitInterruptCounter.Inc(1)
					log.Warn("OPCODE Level interrupt")

					return nil, ErrInterrupt
				}
			default:
			}
		}

		if debug {
			// Capture pre-execution values for tracing.
			logged, pcCopy, gasCopy = false, pc, contract.Gas
		}
		// Get the operation from the jump table and validate the stack to ensure there are
		// enough stack items available to perform the operation.
		op = contract.GetOp(pc)
		operation := in.table[op]
		cost = operation.constantGas // For tracing
		// Validate stack
		if sLen := stack.len(); sLen < operation.minStack {
			return nil, &ErrStackUnderflow{stackLen: sLen, required: operation.minStack}
		} else if sLen > operation.maxStack {
			return nil, &ErrStackOverflow{stackLen: sLen, limit: operation.maxStack}
		}

		if !contract.UseGas(cost) {
			return nil, ErrOutOfGas
		}
		// nolint : nestif
		if operation.dynamicGas != nil {
			// All ops with a dynamic memory usage also has a dynamic gas cost.
			var memorySize uint64
			// calculate the new memory size and expand the memory to fit
			// the operation
			// Memory check needs to be done prior to evaluating the dynamic gas portion,
			// to detect calculation overflows
			if operation.memorySize != nil {
				memSize, overflow := operation.memorySize(stack)
				if overflow {
					return nil, ErrGasUintOverflow
				}
				// memory is expanded in words of 32 bytes. Gas
				// is also calculated in words.
				if memorySize, overflow = math.SafeMul(toWordSize(memSize), 32); overflow {
					return nil, ErrGasUintOverflow
				}
			}
			// Consume the gas and return an error if not enough gas is available.
			// cost is explicitly set so that the capture state defer method can get the proper cost
			var dynamicCost uint64
			dynamicCost, err = operation.dynamicGas(in.evm, contract, stack, mem, memorySize)
			cost += dynamicCost // for tracing

			if err != nil || !contract.UseGas(dynamicCost) {
				return nil, ErrOutOfGas
			}

			if memorySize > 0 {
				mem.Resize(memorySize)
			}
		}

		if debug {
			in.evm.Config.Tracer.CaptureState(pc, op, gasCopy, cost, callContext, in.returnData, in.evm.depth, err)

			logged = true
		}
		// execute the operation
		res, err = operation.execute(&pc, in, callContext)
		if err != nil {
			break
		}

		pc++
	}

	if err == errStopToken {
		err = nil // clear stop token error
	}

	return res, err
}

// nolint: gocognit
// RunWithDelay is Run() with a delay between each opcode. Only used by testcases.
func (in *EVMInterpreter) RunWithDelay(contract *Contract, input []byte, readOnly bool, interruptCtx context.Context, opcodeDelay uint) (ret []byte, err error) {
	// Increment the call depth which is restricted to 1024
	in.evm.depth++
	defer func() { in.evm.depth-- }()

	// Make sure the readOnly is only set if we aren't in readOnly yet.
	// This also makes sure that the readOnly flag isn't removed for child calls.
	if readOnly && !in.readOnly {
		in.readOnly = true
		defer func() { in.readOnly = false }()
	}

	// Reset the previous call's return data. It's unimportant to preserve the old buffer
	// as every returning call will return new data anyway.
	in.returnData = nil

	// Don't bother with the execution if there's no code.
	if len(contract.Code) == 0 {
		return nil, nil
	}

	var (
		op          OpCode        // current opcode
		mem         = NewMemory() // bound memory
		stack       = newstack()  // local stack
		callContext = &ScopeContext{
			Memory:   mem,
			Stack:    stack,
			Contract: contract,
		}
		// For optimisation reason we're using uint64 as the program counter.
		// It's theoretically possible to go above 2^64. The YP defines the PC
		// to be uint256. Practically much less so feasible.
		pc   = uint64(0) // program counter
		cost uint64
		// copies used by tracer
		pcCopy  uint64 // needed for the deferred EVMLogger
		gasCopy uint64 // for EVMLogger to log gas remaining before execution
		logged  bool   // deferred EVMLogger should ignore already logged steps
		res     []byte // result of the opcode execution function
		debug   = in.evm.Config.Tracer != nil
	)
	// Don't move this deferrred function, it's placed before the capturestate-deferred method,
	// so that it gets executed _after_: the capturestate needs the stacks before
	// they are returned to the pools
	defer func() {
		returnStack(stack)
	}()

	contract.Input = input

	if debug {
		defer func() {
			if err != nil {
				if !logged {
					in.evm.Config.Tracer.CaptureState(pcCopy, op, gasCopy, cost, callContext, in.returnData, in.evm.depth, err)
				} else {
					in.evm.Config.Tracer.CaptureFault(pcCopy, op, gasCopy, cost, callContext, in.evm.depth, err)
				}
			}
		}()
	}
	// The Interpreter main run loop (contextual). This loop runs until either an
	// explicit STOP, RETURN or SELFDESTRUCT is executed, an error occurred during
	// the execution of one of the operations or until the done flag is set by the
	// parent context.
	for {
		if interruptCtx != nil {
			// case of interrupting by timeout
			select {
			case <-interruptCtx.Done():
				txHash, _ := GetCurrentTxFromContext(interruptCtx)
				interruptedTxCache, _ := GetCache(interruptCtx)

				if interruptedTxCache == nil {
					break
				}

				// if the tx is already in the cache, it means that it has been interrupted before and we will not interrupt it again
				found, _ := interruptedTxCache.Cache.ContainsOrAdd(txHash, true)
				log.Info("FOUND", "found", found, "txHash", txHash)

				if found {
					interruptedTxCache.Cache.Remove(txHash)
				} else {
					// if the tx is not in the cache, it means that it has not been interrupted before and we will interrupt it
					opcodeCommitInterruptCounter.Inc(1)
					log.Warn("OPCODE Level interrupt")

					return nil, ErrInterrupt
				}
			default:
			}
		}

		time.Sleep(time.Duration(opcodeDelay) * time.Millisecond)

		if debug {
			// Capture pre-execution values for tracing.
			logged, pcCopy, gasCopy = false, pc, contract.Gas
		}
		// Get the operation from the jump table and validate the stack to ensure there are
		// enough stack items available to perform the operation.
		op = contract.GetOp(pc)
		operation := in.table[op]
		cost = operation.constantGas // For tracing
		// Validate stack
		if sLen := stack.len(); sLen < operation.minStack {
			return nil, &ErrStackUnderflow{stackLen: sLen, required: operation.minStack}
		} else if sLen > operation.maxStack {
			return nil, &ErrStackOverflow{stackLen: sLen, limit: operation.maxStack}
		}

		if !contract.UseGas(cost) {
			return nil, ErrOutOfGas
		}

		if operation.dynamicGas != nil {
			// All ops with a dynamic memory usage also has a dynamic gas cost.
			var memorySize uint64
			// calculate the new memory size and expand the memory to fit
			// the operation
			// Memory check needs to be done prior to evaluating the dynamic gas portion,
			// to detect calculation overflows
			if operation.memorySize != nil {
				memSize, overflow := operation.memorySize(stack)
				if overflow {
					return nil, ErrGasUintOverflow
				}
				// memory is expanded in words of 32 bytes. Gas
				// is also calculated in words.
				if memorySize, overflow = math.SafeMul(toWordSize(memSize), 32); overflow {
					return nil, ErrGasUintOverflow
				}
			}
			// Consume the gas and return an error if not enough gas is available.
			// cost is explicitly set so that the capture state defer method can get the proper cost
			var dynamicCost uint64
			dynamicCost, err = operation.dynamicGas(in.evm, contract, stack, mem, memorySize)
			cost += dynamicCost // for tracing

			if err != nil || !contract.UseGas(dynamicCost) {
				return nil, ErrOutOfGas
			}
			// Do tracing before memory expansion
			if debug {
				in.evm.Config.Tracer.CaptureState(pc, op, gasCopy, cost, callContext, in.returnData, in.evm.depth, err)

				logged = true
			}

			if memorySize > 0 {
				mem.Resize(memorySize)
			}
		} else if debug {
			in.evm.Config.Tracer.CaptureState(pc, op, gasCopy, cost, callContext, in.returnData, in.evm.depth, err)

			logged = true
		}
		// execute the operation
		res, err = operation.execute(&pc, in, callContext)
		if err != nil {
			break
		}

		pc++
	}

	if err == errStopToken {
		err = nil // clear stop token error
	}

	return res, err
}
