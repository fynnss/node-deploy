#!/usr/bin/env python3
"""
Inject PQ validator pubkeys into genesis.json at the pqKeyRegistry (0x70)
storage slots, so validators are pre-registered at genesis and
PQRegistryLookup() succeeds from block 0.

Usage:
    python3 tools/inject_pq_genesis.py <genesis.json> <node0_dir> [<node1_dir> ...]

Each <nodeN_dir> must contain:
    - keystore/<file>.json  (validator keystore, has .address field)
    - pq/pubkey.hex         (1312-byte ML-DSA-44 pubkey, hex-encoded)

The script:
1. Reads the validator consensus address from keystore
2. Reads the PQ pubkey from pq/pubkey.hex
3. Computes the 41 storage slots at 0x70 per pqRegistrySlot(addr, i) = keccak256(addr || uint256(i))
4. Injects them into genesis.json alloc["0x0000...0070"].storage
"""
import json
import glob
import sys
import os

from web3 import Web3


def keccak256(data: bytes) -> bytes:
    return bytes(Web3.keccak(data))


PQ_REGISTRY_ADDR = "0x0000000000000000000000000000000000000070"
PQ_PUBKEY_SIZE = 1312
PQ_SLOTS_PER_KEY = 41  # ceil(1312 / 32)


def pq_registry_slot(addr_bytes: bytes, index: int) -> str:
    """Reproduce pqRegistrySlot: keccak256(addr(20) || uint256(index))"""
    index_bytes = index.to_bytes(32, byteorder="big")
    slot = keccak256(addr_bytes + index_bytes)
    return "0x" + slot.hex()


def pubkey_to_storage(addr_hex: str, pubkey_hex: str) -> dict:
    """Return {slot_hex: value_hex} for 41 slots of a 1312-byte pubkey."""
    addr_bytes = bytes.fromhex(addr_hex.replace("0x", "").lower())
    pubkey_bytes = bytes.fromhex(pubkey_hex.strip())
    assert len(pubkey_bytes) == PQ_PUBKEY_SIZE, f"bad pubkey len: {len(pubkey_bytes)}"

    storage = {}
    for i in range(PQ_SLOTS_PER_KEY):
        start = i * 32
        end = start + 32
        chunk = pubkey_bytes[start:end] if start < len(pubkey_bytes) else b"\x00" * 32
        if len(chunk) < 32:
            chunk = chunk + b"\x00" * (32 - len(chunk))

        slot = pq_registry_slot(addr_bytes, i)
        value = "0x" + chunk.hex()
        storage[slot] = value
    return storage


def get_validator_addr(node_dir: str) -> str:
    """Read consensus address from keystore JSON."""
    keystore_files = glob.glob(os.path.join(node_dir, "keystore", "*"))
    if not keystore_files:
        raise FileNotFoundError(f"No keystore found in {node_dir}/keystore/")
    with open(keystore_files[0]) as f:
        ks = json.load(f)
    return "0x" + ks["address"]


def get_pq_pubkey(node_dir: str) -> str:
    """Read PQ pubkey hex from pq/pubkey.hex."""
    path = os.path.join(node_dir, "pq", "pubkey.hex")
    with open(path) as f:
        return f.read().strip()


def main():
    if len(sys.argv) < 3:
        print(f"Usage: {sys.argv[0]} <genesis.json> <node0_dir> [<node1_dir> ...]",
              file=sys.stderr)
        sys.exit(1)

    genesis_path = sys.argv[1]
    node_dirs = sys.argv[2:]

    with open(genesis_path) as f:
        genesis = json.load(f)

    alloc = genesis.setdefault("alloc", {})

    # Normalise the registry address key (genesis may use mixed case).
    registry_key = None
    for k in alloc:
        if k.lower().replace("0x", "") == "70":
            registry_key = k
            break
    if registry_key is None:
        registry_key = PQ_REGISTRY_ADDR
        alloc[registry_key] = {"balance": "0x0"}

    registry_entry = alloc[registry_key]
    storage = registry_entry.setdefault("storage", {})

    for node_dir in node_dirs:
        addr = get_validator_addr(node_dir)
        pubkey = get_pq_pubkey(node_dir)
        slots = pubkey_to_storage(addr, pubkey)
        storage.update(slots)
        print(f"  registered {addr} -> {len(slots)} slots", file=sys.stderr)

    with open(genesis_path, "w") as f:
        json.dump(genesis, f, indent=2)
        f.write("\n")

    print(f"Injected {len(node_dirs)} validator PQ pubkeys into {genesis_path}",
          file=sys.stderr)


if __name__ == "__main__":
    main()
