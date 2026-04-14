#!/usr/bin/env bash

# Exit script on error
set -e

basedir=$(
  cd $(dirname $0)
  pwd
)
workspace=${basedir}
source ${workspace}/.env
size=$((BSC_CLUSTER_SIZE))
stateScheme="hash"
dbEngine="leveldb"
gcmode="full"
sleepBeforeStart=15
sleepAfterStart=10

# stop geth client
function exit_previous() {
  ValIdx=$1
  ps -ef | grep geth$ValIdx | grep config | awk '{print $2}' | xargs kill
  sleep ${sleepBeforeStart}
}

function create_validator() {
  rm -rf ${workspace}/.local
  mkdir -p ${workspace}/.local

  # Ensure gen-pq-account is built (deterministic PQ validator keys).
  if [ ! -x "${workspace}/tools/gen-pq-account/gen-pq-account" ]; then
    (cd ${workspace}/tools/gen-pq-account && go build .)
  fi

  for ((i = 0; i < size; i++)); do
    cp -r ${workspace}/keys/validator${i} ${workspace}/.local/
    cp -r ${workspace}/keys/bls${i} ${workspace}/.local/

    # Generate a per-validator ML-DSA-44 key from a deterministic seed so the
    # same cluster layout always produces the same PQ vote identities.
    # gen-pq-account writes both hex and raw-binary forms; geth --pqvotekey
    # consumes privkey.bin directly.
    pq_seed=$(printf 'pq-validator-%d' ${i} | xxd -p -c 256)
    pq_outdir=${workspace}/.local/pq${i}
    rm -rf ${pq_outdir}
    ${workspace}/tools/gen-pq-account/gen-pq-account \
      -seed ${pq_seed} \
      -out ${pq_outdir} >/dev/null
  done
}

function prepare_bsc_client() {
  if [ ${useLatestBscClient} = true ]; then
    if [ ! -f "${workspace}/bsc/Makefile" ]; then
      cd ${workspace}
      git clone https://github.com/bnb-chain/bsc.git
    fi
    cd ${workspace}/bsc && git pull && make geth && mv -f ${workspace}/bsc/build/bin/geth ${workspace}/bin/
  fi
}
# reset genesis, but keep edited genesis-template.json
function reset_genesis() {
  if [ ! -f "${workspace}/genesis/genesis-template.json" ]; then
    cd ${workspace} && git submodule update --init --recursive genesis
    cd ${workspace}/genesis && git reset --hard ${GENESIS_COMMIT}
  fi
  cd ${workspace}/genesis
  cp genesis-template.json genesis-template.json.bk
  cp scripts/init_holders.template scripts/init_holders.template.bk
  git stash
  cd ${workspace} && git submodule update --remote --recursive genesis && cd ${workspace}/genesis
  git reset --hard ${GENESIS_COMMIT}
  mv genesis-template.json.bk genesis-template.json
  mv scripts/init_holders.template.bk scripts/init_holders.template

  poetry install --no-root
  npm install
  rm -rf lib/forge-std
  forge install --no-git foundry-rs/forge-std@v1.7.3
  cd lib/forge-std/lib
  rm -rf ds-test
  git clone https://github.com/dapphub/ds-test
}

function prepare_config() {
  rm -f ${workspace}/genesis/validators.conf

  passedHardforkTime=$(expr $(date +%s) + ${PASSED_FORK_DELAY})
  echo "passedHardforkTime "${passedHardforkTime} >${workspace}/.local/hardforkTime.txt
  initHolders=${INIT_HOLDER}
  for ((i = 0; i < size; i++)); do
    for f in ${workspace}/.local/validator${i}/keystore/*; do
      cons_addr="0x$(cat ${f} | jq -r .address)"
      initHolders=${initHolders}","${cons_addr}
      fee_addr=${cons_addr}
    done

    targetDir=${workspace}/.local/node${i}
    mkdir -p ${targetDir} && cd ${targetDir}
    cp ${workspace}/keys/password.txt ./
    cp ${workspace}/.local/hardforkTime.txt ./
    bbcfee_addrs=${fee_addr}
    powers="0x000001d1a94a2000" #2000000000000
    mv ${workspace}/.local/bls${i}/bls ./ && rm -rf ${workspace}/.local/bls${i}
    vote_addr=0x$(cat ./bls/keystore/*json | jq .pubkey | sed 's/"//g')
    echo "${cons_addr},${bbcfee_addrs},${fee_addr},${powers},${vote_addr}" >>${workspace}/genesis/validators.conf

    # Copy keystore into node dir early so inject_pq_genesis.py can read
    # the consensus address before initNetwork moves the original.
    cp -r ${workspace}/.local/validator${i}/keystore ./

    # Stage the PQ vote key under the validator datadir so --pqvotekey can
    # pick it up at startup. ML-DSA-44 replaces BLS vote signing post-PQFork.
    mkdir -p ./pq
    mv ${workspace}/.local/pq${i}/privkey.bin ./pq/privkey.bin
    mv ${workspace}/.local/pq${i}/pubkey.bin  ./pq/pubkey.bin
    mv ${workspace}/.local/pq${i}/pubkey.hex  ./pq/pubkey.hex
    mv ${workspace}/.local/pq${i}/address.txt ./pq/address.txt
    rm -rf ${workspace}/.local/pq${i}
    if [ ${EnableSentryNode} = true ]; then
      mkdir -p ${workspace}/.local/sentry${i}
    fi
  done
  if [ ${EnableFullNode} = true ]; then
    mkdir -p ${workspace}/.local/fullnode0
  fi
  rm -f ${workspace}/.local/hardforkTime.txt

  cd ${workspace}/genesis/
  git checkout HEAD contracts
  sed -i -e 's/alreadyInit = true;/turnLength = 16;alreadyInit = true;/' ${workspace}/genesis/contracts/BSCValidatorSet.sol
  sed -i -e 's/public onlyCoinbase onlyZeroGasPrice {/public onlyCoinbase onlyZeroGasPrice {if (block.number < 2000) return;/' ${workspace}/genesis/contracts/BSCValidatorSet.sol

  poetry run python -m scripts.generate generate-validators
  poetry run python -m scripts.generate generate-init-holders "${initHolders}"
  poetry run python -m scripts.generate dev \
    --dev-chain-id "${CHAIN_ID}" \
    --init-burn-ratio "1000" \
    --init-felony-slash-scope "60" \
    --breathe-block-interval "10 minutes" \
    --block-interval "3 seconds" \
    --stake-hub-protector "${INIT_HOLDER}" \
    --unbond-period "2 minutes" \
    --downtime-jail-time "2 minutes" \
    --felony-jail-time "3 minutes" \
    --misdemeanor-threshold "50" \
    --felony-threshold "150" \
    --init-voting-period "2 minutes / BLOCK_INTERVAL" \
    --init-min-period-after-quorum "uint64(1 minutes / BLOCK_INTERVAL)" \
    --governor-protector "${INIT_HOLDER}" \
    --init-minimal-delay "1 minutes" \
    --token-recover-portal-protector "${INIT_HOLDER}"
  cp genesis-dev.json genesis.json

  # Pre-populate the pqKeyRegistry (0x70) storage in genesis so that every
  # validator's ML-DSA-44 pubkey is available from block 0.  Without this the
  # PQ vote manager cannot match its signer key to an active validator.
  node_dirs=""
  for ((i = 0; i < size; i++)); do
    node_dirs="${node_dirs} ${workspace}/.local/node${i}"
  done
  poetry run python3 ${workspace}/tools/inject_pq_genesis.py \
    ${workspace}/genesis/genesis.json ${node_dirs}
}

function initNetwork() {
  cd ${workspace}
  for ((i = 0; i < size; i++)); do
    mkdir ${workspace}/.local/node${i}/geth
    cp ${workspace}/keys/validator-nodekey${i} ${workspace}/.local/node${i}/geth/nodekey
    rm -rf ${workspace}/.local/node${i}/keystore && mv ${workspace}/.local/validator${i}/keystore ${workspace}/.local/node${i}/ && rm -rf ${workspace}/.local/validator${i}
    if [ ${EnableSentryNode} = true ]; then
      mkdir ${workspace}/.local/sentry${i}/geth
      cp ${workspace}/keys/sentry-nodekey${i} ${workspace}/.local/sentry${i}/geth/nodekey
    fi
  done
  if [ ${EnableFullNode} = true ]; then
    mkdir ${workspace}/.local/fullnode0/geth
    cp ${workspace}/keys/fullnode-nodekey0 ${workspace}/.local/fullnode0/geth/nodekey
  fi

  init_extra_args=""
  if [ ${EnableSentryNode} = true ]; then
    init_extra_args="--init.sentrynode-size ${size} --init.sentrynode-ports 30411"
  fi
  if [ ${EnableFullNode} = true ]; then
    init_extra_args="${init_extra_args} --init.fullnode-size 1 --init.fullnode-ports 30511"
  fi
  if [ "${RegisterNodeID}" = true ]; then
    if [ "${EnableSentryNode}" = true ]; then
      init_extra_args="${init_extra_args} --init.evn-sentry-register"
    else
      init_extra_args="${init_extra_args} --init.evn-validator-register"
    fi
  fi
  if [ "${EnableEVNWhitelist}" = true ]; then
    if [ "${EnableSentryNode}" = true ]; then
      init_extra_args="${init_extra_args} --init.evn-sentry-whitelist"
    else
      init_extra_args="${init_extra_args} --init.evn-validator-whitelist"
    fi
  fi
  ${workspace}/bin/geth init-network --init.dir ${workspace}/.local --init.size=${size} --config ${workspace}/config.toml ${init_extra_args} ${workspace}/genesis/genesis.json
  rm -f ${workspace}/*bsc.log*
  for ((i = 0; i < size; i++)); do
    sed -i -e '/"<nil>"/d' ${workspace}/.local/node${i}/config.toml
    # init genesis
    initLog=${workspace}/.local/node${i}/init.log
    if [ $i -eq 0 ]; then
      ${workspace}/bin/geth --datadir ${workspace}/.local/node${i} init --state.scheme ${stateScheme} --db.engine ${dbEngine} ${workspace}/genesis/genesis.json >"${initLog}" 2>&1
    else
      ${workspace}/bin/geth --datadir ${workspace}/.local/node${i} init --state.scheme path --db.engine pebble ${workspace}/genesis/genesis.json >"${initLog}" 2>&1
    fi
    rm -f ${workspace}/.local/node${i}/*bsc.log*

    if [ ${EnableSentryNode} = true ]; then
      sed -i -e '/"<nil>"/d' ${workspace}/.local/sentry${i}/config.toml
      initLog=${workspace}/.local/sentry${i}/init.log
      ${workspace}/bin/geth --datadir ${workspace}/.local/sentry${i} init --state.scheme path --db.engine pebble ${workspace}/genesis/genesis.json >"${initLog}" 2>&1
      rm -f ${workspace}/.local/sentry${i}/*bsc.log*
    fi
  done
  if [ ${EnableFullNode} = true ]; then
    sed -i -e '/"<nil>"/d' ${workspace}/.local/fullnode0/config.toml
    sed -i -e 's/EnableEVNFeatures = true/EnableEVNFeatures = false/g' ${workspace}/.local/fullnode0/config.toml
    initLog=${workspace}/.local/fullnode0/init.log
    ${workspace}/bin/geth --datadir ${workspace}/.local/fullnode0 init --state.scheme path --db.engine pebble ${workspace}/genesis/genesis.json >"${initLog}" 2>&1
    rm -f ${workspace}/.local/fullnode0/*bsc.log*
  fi
}

function start_node() {
  local type=$1 # node | sentry | full
  local idx=$2  # index (validator/sentry)，full default 0
  local datadir=$3
  local geth_bin=$4
  local cons_addr=$5
  local http_port=$6
  local ws_port=$7
  local metrics_port=$8
  local pprof_port=$9

  # update `config` in genesis.json
  # ${workspace}/.local/node${i}/geth${i} dumpgenesis --datadir ${workspace}/.local/node${i} | jq . > ${workspace}/.local/node${i}/genesis.json
  nohup ${geth_bin} --config ${datadir}/config.toml \
    --datadir ${datadir} \
    --nodekey ${datadir}/geth/nodekey \
    --rpc.allow-unprotected-txs --allow-insecure-unlock \
    --ws --ws.addr 0.0.0.0 --ws.port ${ws_port} \
    --http --http.addr 0.0.0.0 --http.port ${http_port} --http.corsdomain "*" \
    --metrics --metrics.addr localhost --metrics.port ${metrics_port} \
    --pprof --pprof.addr localhost --pprof.port ${pprof_port} \
    --gcmode ${gcmode} --syncmode full --monitor.maliciousvote \
    --rialtohash ${rialtoHash} \
    --override.passedforktime ${PassedForkTime} \
    --override.lorentz ${PassedForkTime} \
    --override.maxwell ${PassedForkTime} \
    --override.fermi ${LastHardforkTime} \
    --override.osaka ${LastHardforkTime} \
    --override.mendel ${LastHardforkTime} \
    --override.pasteur ${LastHardforkTime} \
    --override.pqhardfork ${LastHardforkTime} \
    --override.immutabilitythreshold ${FullImmutabilityThreshold} \
    --override.breatheblockinterval ${BreatheBlockInterval} \
    --override.minforblobrequest ${MinBlocksForBlobRequests} \
    --override.defaultextrareserve ${DefaultExtraReserveForBlobRequests} \
    $([ "${type}" = "node" ] && echo "--mine --pqvotekey ${datadir}/pq/privkey.bin --unlock ${cons_addr} --miner.etherbase ${cons_addr} --password ${datadir}/password.txt") \
    >>${datadir}/bsc-node.log 2>&1 &
}

function native_start() {
  PassedForkTime=$(cat ${workspace}/.local/node0/hardforkTime.txt | grep passedHardforkTime | awk -F" " '{print $NF}')
  LastHardforkTime=$(expr ${PassedForkTime} + ${LAST_FORK_MORE_DELAY})
  rialtoHash=$(cat ${workspace}/.local/node0/init.log | grep "database=chaindata" | awk -F"=" '{print $NF}' | awk -F'"' '{print $1}')

  for ((i = 0; i < size; i++)); do
    datadir="${workspace}/.local/node${i}"

    # optional: ValIdx filtering
    if [ ! -z "$1" ] && [ $i -ne $1 ]; then
      continue
    fi

    # get validator address
    cons_addr="0x$(jq -r .address ${datadir}/keystore/*)"

    cp ${workspace}/bin/geth ${datadir}/geth${i}

    base=$((8545 + i * 2))
    start_node "node" $i $datadir "${datadir}/geth${i}" "${cons_addr}" \
      $base $base $((6060 + i * 2)) $((7060 + i * 2))
  done

  if [ ${EnableSentryNode} = true ]; then
    sleep 10
    for ((i = 0; i < size; i++)); do
      datadir="${workspace}/.local/sentry${i}"
      cp ${workspace}/bin/geth ${datadir}/geth${i}

      base=$((8545 + i * 2))
      start_node "sentry" $i $datadir "${datadir}/geth${i}" "" \
        $((base + 1)) $((base + 1)) $((6060 + i * 2 + 1)) $((7060 + i * 2 + 1))
    done
  fi

  if [ ${EnableFullNode} = true ]; then
    datadir="${workspace}/.local/fullnode0"
    cp ${workspace}/bin/geth ${datadir}/geth0

    start_node "full" 0 $datadir "${datadir}/geth0" "" \
      8645 8645 6160 7160
  fi

  sleep ${sleepAfterStart}
}

function register_stakehub() {
  # wait feynman enable
  sleep 45
  for ((i = 0; i < size; i++)); do
    ${workspace}/create-validator/create-validator --consensus-key-dir ${workspace}/keys/validator${i} --vote-key-dir ${workspace}/keys/bls${i} \
      --password-path ${workspace}/keys/password.txt --amount 20001 --validator-desc Val${i} --rpc-url ${RPC_URL}
  done
}

CMD=$1
ValidatorIdx=$2
case ${CMD} in
reset)
  exit_previous
  create_validator
  prepare_bsc_client
  reset_genesis
  prepare_config
  initNetwork
  native_start
  register_stakehub
  ;;
stop)
  exit_previous $ValidatorIdx
  ;;
start)
  native_start $ValidatorIdx
  ;;
restart)
  exit_previous $ValidatorIdx
  native_start $ValidatorIdx
  ;;
*)
  echo "Usage: bsc_cluster.sh | reset | stop [vidx]| start [vidx]| restart [vidx]"
  ;;
esac
