use cosmwasm_std::testing::{mock_dependencies, mock_env};
use cosmwasm_std::{Addr, Empty, HexBinary, StdResult};
use cw_multi_test::{App, AppResponse, Contract, ContractWrapper, Executor};
use gateway_registry::contract::{execute, instantiate, migrate, query, validate_config};
use gateway_registry::msg::{ExecuteMsg, InstantiateMsg, MigrateMsg, QueryMsg};
use gateway_registry::state::{Config, Instance, WritersSource, BY_HEIGHT, INSTANCES, WRITERS};
use gateway_registry::ContractError;

const TTL: u64 = 16200;
const MIN_INTERVAL: u64 = 100;

fn contract() -> Box<dyn Contract<Empty>> {
    Box::new(ContractWrapper::new(execute, instantiate, query))
}

struct Suite {
    app: App,
    contract: Addr,
    owner: Addr,
}

impl Suite {
    fn new(writers: &[&Addr]) -> Self {
        let mut app = App::default();
        let owner = app.api().addr_make("owner");
        let code_id = app.store_code(contract());
        let contract = app
            .instantiate_contract(
                code_id,
                owner.clone(),
                &InstantiateMsg {
                    owner: owner.to_string(),
                    writers: writers.iter().map(|w| w.to_string()).collect(),
                    max_endpoint_len: 256,
                    max_topics: 16,
                    max_instances_per_address: 16,
                    min_update_interval_blocks: MIN_INTERVAL,
                },
                &[],
                "gateway-registry",
                Some(owner.to_string()),
            )
            .unwrap();
        Suite {
            app,
            contract,
            owner,
        }
    }

    fn exec(&mut self, sender: &Addr, msg: &ExecuteMsg) -> StdResult<AppResponse> {
        self.app
            .execute_contract(sender.clone(), self.contract.clone(), msg, &[])
    }

    fn register(&mut self, sender: &Addr, instance_id: &str) -> StdResult<AppResponse> {
        self.exec(sender, &register_msg(instance_id))
    }

    fn advance(&mut self, blocks: u64) {
        self.app.update_block(|block| block.height += blocks);
    }

    fn height(&self) -> u64 {
        self.app.block_info().height
    }

    fn member(&self, address: &Addr) -> Vec<Instance> {
        self.app
            .wrap()
            .query_wasm_smart(
                &self.contract,
                &QueryMsg::Member {
                    address: address.to_string(),
                },
            )
            .unwrap()
    }

    fn member_ids(&self, address: &Addr) -> Vec<String> {
        self.member(address)
            .into_iter()
            .map(|instance| instance.instance_id)
            .collect()
    }

    fn stored_instances(&self) -> Vec<(u64, Addr, String)> {
        let storage = self.app.contract_storage(&self.contract);
        INSTANCES
            .range(&*storage, None, None, cosmwasm_std::Order::Ascending)
            .map(|item| {
                let ((address, id), instance) = item.unwrap();
                (instance.updated_height, address, id)
            })
            .collect()
    }

    fn index_rows(&self) -> Vec<(u64, Addr, String)> {
        let storage = self.app.contract_storage(&self.contract);
        BY_HEIGHT
            .keys(&*storage, None, None, cosmwasm_std::Order::Ascending)
            .map(|key| key.unwrap())
            .collect()
    }

    fn assert_index_consistent(&self) {
        let mut stored = self.stored_instances();
        stored.sort();
        assert_eq!(stored, self.index_rows());
    }
}

fn register_msg(instance_id: &str) -> ExecuteMsg {
    ExecuteMsg::RegisterInstance {
        instance_id: instance_id.to_string(),
        endpoint: "https://gw.example.com:8443".to_string(),
        spki_sha256: HexBinary::from([7u8; 32]),
        protocol: 1,
        topics: vec!["load@1".to_string()],
    }
}

fn register_with(endpoint: &str, spki_sha256: HexBinary, topics: Vec<String>) -> ExecuteMsg {
    ExecuteMsg::RegisterInstance {
        instance_id: "a".to_string(),
        endpoint: endpoint.to_string(),
        spki_sha256,
        protocol: 1,
        topics,
    }
}

fn assert_err(result: StdResult<AppResponse>, expected: ContractError) {
    let err = result.unwrap_err();
    assert!(
        err.to_string().contains(&expected.to_string()),
        "unexpected error: {err}"
    );
}

fn expired_ids(response: &AppResponse) -> Vec<String> {
    response
        .events
        .iter()
        .filter(|event| event.ty == "wasm-instance_expired")
        .map(|event| {
            event
                .attributes
                .iter()
                .find(|attr| attr.key == "instance_id")
                .unwrap()
                .value
                .clone()
        })
        .collect()
}

fn addr(name: &str) -> Addr {
    App::default().api().addr_make(name)
}

/// A sender outside the writers list cannot register.
#[test]
fn non_writer_rejected() {
    let writer = addr("writer");
    let stranger = addr("stranger");
    let mut suite = Suite::new(&[&writer]);
    assert_err(suite.register(&stranger, "a"), ContractError::NotAWriter {});
    assert!(suite.member(&stranger).is_empty());
}

/// SyncWriters, SetPaused and SetOwner refuse everyone but the owner, and SetOwner hands the role over.
#[test]
fn owner_only_commands() {
    let writer = addr("writer");
    let new_owner = addr("new_owner");
    let mut suite = Suite::new(&[&writer]);
    for msg in [
        ExecuteMsg::SyncWriters { writers: vec![] },
        ExecuteMsg::SetPaused { paused: true },
        ExecuteMsg::SetOwner {
            owner: writer.to_string(),
        },
    ] {
        assert_err(suite.exec(&writer, &msg), ContractError::Unauthorized {});
    }

    let owner = suite.owner.clone();
    suite
        .exec(
            &owner,
            &ExecuteMsg::SetOwner {
                owner: new_owner.to_string(),
            },
        )
        .unwrap();
    let config: Config = suite
        .app
        .wrap()
        .query_wasm_smart(&suite.contract, &QueryMsg::Config {})
        .unwrap();
    assert_eq!(config.owner, new_owner);
    assert_err(
        suite.exec(&owner, &ExecuteMsg::SetPaused { paused: true }),
        ContractError::Unauthorized {},
    );
    suite
        .exec(&new_owner, &ExecuteMsg::SetPaused { paused: true })
        .unwrap();
}

/// Instantiate fills the config with the design defaults.
#[test]
fn config_defaults() {
    let suite = Suite::new(&[]);
    let config: Config = suite
        .app
        .wrap()
        .query_wasm_smart(&suite.contract, &QueryMsg::Config {})
        .unwrap();
    assert_eq!(
        config,
        Config {
            owner: suite.owner.clone(),
            paused: false,
            max_endpoint_len: 256,
            max_topics: 16,
            max_instances_per_address: 16,
            min_update_interval_blocks: 100,
            instance_ttl_blocks: 16200,
            prune_per_write: 10,
            writers_source: WritersSource::ContractCopy,
        }
    );
}

/// While paused RegisterInstance is refused and queries still answer.
#[test]
fn paused_refuses_register_only() {
    let writer = addr("writer");
    let mut suite = Suite::new(&[&writer]);
    suite.register(&writer, "a").unwrap();
    let owner = suite.owner.clone();
    suite
        .exec(&owner, &ExecuteMsg::SetPaused { paused: true })
        .unwrap();
    assert_err(suite.register(&writer, "b"), ContractError::Paused {});
    assert_eq!(suite.member_ids(&writer), ["a"]);
    suite
        .exec(&owner, &ExecuteMsg::SetPaused { paused: false })
        .unwrap();
    suite.register(&writer, "b").unwrap();
}

/// SyncWriters replaces the list and deletes every instance of a dropped writer, index rows included.
#[test]
fn sync_writers_removes_instances_of_dropped_writers() {
    let kept = addr("kept");
    let dropped = addr("dropped");
    let added = addr("added");
    let mut suite = Suite::new(&[&kept, &dropped]);
    suite.register(&kept, "a").unwrap();
    suite.register(&dropped, "a").unwrap();
    suite.register(&dropped, "b").unwrap();

    let owner = suite.owner.clone();
    let response = suite
        .exec(
            &owner,
            &ExecuteMsg::SyncWriters {
                writers: vec![kept.to_string(), added.to_string()],
            },
        )
        .unwrap();
    assert!(response
        .events
        .iter()
        .any(|e| e.ty == "wasm-writers_synced"));

    assert!(suite.member(&dropped).is_empty());
    assert_eq!(suite.member_ids(&kept), ["a"]);
    assert_eq!(suite.stored_instances().len(), 1);
    suite.assert_index_consistent();
    assert_err(suite.register(&dropped, "c"), ContractError::NotAWriter {});
    suite.register(&added, "a").unwrap();
    let storage = suite.app.contract_storage(&suite.contract);
    assert!(!WRITERS.has(&*storage, &dropped));
}

/// One instance can update again only after min_update_interval_blocks.
#[test]
fn per_instance_rate_limit() {
    let writer = addr("writer");
    let mut suite = Suite::new(&[&writer]);
    suite.register(&writer, "a").unwrap();
    let next_height = suite.height() + MIN_INTERVAL;
    suite.advance(MIN_INTERVAL - 1);
    assert_err(
        suite.register(&writer, "a"),
        ContractError::TooSoon { next_height },
    );
    suite.advance(1);
    suite.register(&writer, "a").unwrap();
    assert_eq!(suite.member(&writer)[0].updated_height, next_height);
}

/// Two replicas of one address register in the same block.
#[test]
fn two_replicas_register_in_same_block() {
    let writer = addr("writer");
    let mut suite = Suite::new(&[&writer]);
    suite.register(&writer, "replica-1").unwrap();
    suite.register(&writer, "replica-2").unwrap();
    assert_eq!(suite.member_ids(&writer), ["replica-1", "replica-2"]);
}

/// A 17th live instance is refused while the 16 existing ones can still refresh.
#[test]
fn seventeenth_instance_rejected() {
    let writer = addr("writer");
    let mut suite = Suite::new(&[&writer]);
    for i in 0..16 {
        suite.register(&writer, &format!("i-{i}")).unwrap();
    }
    assert_err(
        suite.register(&writer, "i-16"),
        ContractError::TooManyInstances { max: 16 },
    );
    suite.advance(MIN_INTERVAL);
    suite.register(&writer, "i-0").unwrap();
    assert_eq!(suite.member(&writer).len(), 16);
}

/// A 17th instance is accepted when one of the 16 is expired, and it replaces the expired one.
#[test]
fn seventeenth_accepted_when_one_expired_and_replaces_it() {
    let writer = addr("writer");
    let mut suite = Suite::new(&[&writer]);
    suite.register(&writer, "old").unwrap();
    suite.advance(MIN_INTERVAL);
    for i in 1..16 {
        suite.register(&writer, &format!("i-{i}")).unwrap();
    }
    suite.advance(TTL - MIN_INTERVAL);

    let response = suite.register(&writer, "new").unwrap();
    assert_eq!(expired_ids(&response), ["old"]);
    let ids = suite.member_ids(&writer);
    assert_eq!(ids.len(), 16);
    assert!(ids.contains(&"new".to_string()));
    assert!(!ids.contains(&"old".to_string()));
    assert_eq!(suite.stored_instances().len(), 16);
    suite.assert_index_consistent();
}

/// Member, MembersOf and Members all skip an instance from the block it expires, before any prune.
#[test]
fn queries_do_not_return_expired_instance() {
    let writer = addr("writer");
    let mut suite = Suite::new(&[&writer]);
    suite.register(&writer, "a").unwrap();
    suite.advance(TTL - 1);
    assert_eq!(suite.member_ids(&writer), ["a"]);

    suite.advance(1);
    assert!(suite.member(&writer).is_empty());
    let of: Vec<(String, Vec<Instance>)> = suite
        .app
        .wrap()
        .query_wasm_smart(
            &suite.contract,
            &QueryMsg::MembersOf {
                addresses: vec![writer.to_string()],
            },
        )
        .unwrap();
    assert_eq!(of, vec![(writer.to_string(), vec![])]);
    let members: Vec<(String, Instance)> = suite
        .app
        .wrap()
        .query_wasm_smart(
            &suite.contract,
            &QueryMsg::Members {
                start_after: None,
                limit: None,
            },
        )
        .unwrap();
    assert!(members.is_empty());
    assert_eq!(suite.stored_instances().len(), 1);
}

/// RegisterInstance deletes at most 10 expired instances of any address, oldest first.
#[test]
fn register_prunes_at_most_ten_oldest_first() {
    let first = addr("first");
    let second = addr("second");
    let third = addr("third");
    let mut suite = Suite::new(&[&first, &second, &third]);
    for i in 0..12 {
        let sender = if i % 2 == 0 { &first } else { &second };
        suite.register(sender, &format!("i-{i}")).unwrap();
        suite.advance(1);
    }
    suite.advance(TTL);

    let response = suite.register(&third, "a").unwrap();
    let expected: Vec<String> = (0..10).map(|i| format!("i-{i}")).collect();
    assert_eq!(expired_ids(&response), expected);
    let remaining: Vec<String> = suite.index_rows().into_iter().map(|row| row.2).collect();
    assert_eq!(remaining, ["i-10", "i-11", "a"]);
    suite.assert_index_consistent();
}

/// Replacing the sender's own expired instance counts toward the 10 deletes of one RegisterInstance.
#[test]
fn replacement_counts_toward_prune_budget() {
    let writer = addr("writer");
    let mut suite = Suite::new(&[&writer]);
    for i in 0..16 {
        suite.register(&writer, &format!("i-{i}")).unwrap();
        suite.advance(1);
    }
    suite.advance(TTL);

    let response = suite.register(&writer, "new").unwrap();
    let mut expected = vec!["i-0".to_string()];
    expected.extend((1..10).map(|i| format!("i-{i}")));
    assert_eq!(expired_ids(&response), expected);
    assert_eq!(suite.stored_instances().len(), 7);
    suite.assert_index_consistent();
}

/// PruneExpired refuses a non-writer and deletes no more than its limit.
#[test]
fn prune_expired_writer_only_and_respects_limit() {
    let writer = addr("writer");
    let stranger = addr("stranger");
    let mut suite = Suite::new(&[&writer]);
    for i in 0..5 {
        suite.register(&writer, &format!("i-{i}")).unwrap();
        suite.advance(1);
    }
    suite.advance(TTL);

    assert_err(
        suite.exec(&stranger, &ExecuteMsg::PruneExpired { limit: 10 }),
        ContractError::NotAWriter {},
    );
    let response = suite
        .exec(&writer, &ExecuteMsg::PruneExpired { limit: 2 })
        .unwrap();
    assert_eq!(expired_ids(&response), ["i-0", "i-1"]);
    assert_eq!(suite.stored_instances().len(), 3);
    let response = suite
        .exec(&writer, &ExecuteMsg::PruneExpired { limit: 10 })
        .unwrap();
    assert_eq!(expired_ids(&response).len(), 3);
    assert!(suite.index_rows().is_empty());
}

/// PruneExpired caps its limit at 100 and never touches a live instance.
#[test]
fn prune_expired_caps_limit_and_keeps_live() {
    let writers: Vec<Addr> = (0..7).map(|i| addr(&format!("writer{i}"))).collect();
    let refs: Vec<&Addr> = writers.iter().collect();
    let mut suite = Suite::new(&refs);
    for writer in &writers {
        for i in 0..16 {
            suite.register(writer, &format!("i-{i}")).unwrap();
        }
    }
    suite.advance(TTL);
    let live = addr("writer0");
    suite.register(&live, "i-0").unwrap();
    let before = suite.stored_instances().len();

    let response = suite
        .exec(&live, &ExecuteMsg::PruneExpired { limit: u32::MAX })
        .unwrap();
    assert_eq!(expired_ids(&response).len(), 100);
    assert_eq!(suite.stored_instances().len(), before - 100);
    let response = suite
        .exec(&live, &ExecuteMsg::PruneExpired { limit: u32::MAX })
        .unwrap();
    assert!(!expired_ids(&response).is_empty());
    assert_eq!(suite.member_ids(&live), ["i-0"]);
}

/// A refresh before the TTL restarts the expiry count.
#[test]
fn refresh_before_ttl_keeps_entry_alive() {
    let writer = addr("writer");
    let mut suite = Suite::new(&[&writer]);
    suite.register(&writer, "a").unwrap();
    suite.advance(TTL - 1);
    suite.register(&writer, "a").unwrap();
    suite.advance(TTL - 1);
    assert_eq!(suite.member_ids(&writer), ["a"]);
    suite.advance(1);
    assert!(suite.member(&writer).is_empty());
}

/// BY_HEIGHT keeps exactly one row per instance after update, deregister, prune and replacement.
#[test]
fn by_height_stays_consistent() {
    let writer = addr("writer");
    let other = addr("other");
    let mut suite = Suite::new(&[&writer, &other]);
    suite.register(&writer, "a").unwrap();
    suite.register(&writer, "b").unwrap();
    suite.register(&other, "c").unwrap();
    suite.assert_index_consistent();
    assert_eq!(suite.index_rows().len(), 3);

    suite.advance(MIN_INTERVAL);
    suite.register(&writer, "a").unwrap();
    suite.assert_index_consistent();
    assert_eq!(suite.index_rows().len(), 3);

    suite
        .exec(
            &writer,
            &ExecuteMsg::DeregisterInstance {
                instance_id: "b".to_string(),
            },
        )
        .unwrap();
    suite.assert_index_consistent();
    assert_eq!(suite.index_rows().len(), 2);

    suite.advance(TTL);
    suite
        .exec(&writer, &ExecuteMsg::PruneExpired { limit: 1 })
        .unwrap();
    suite.assert_index_consistent();
    assert_eq!(suite.index_rows().len(), 1);

    suite.register(&other, "c").unwrap();
    suite.assert_index_consistent();
    assert_eq!(suite.index_rows().len(), 1);
}

/// DeregisterInstance removes only the sender's own instance and is a no-op on an unknown id.
#[test]
fn deregister_affects_own_instance_only() {
    let writer = addr("writer");
    let other = addr("other");
    let mut suite = Suite::new(&[&writer, &other]);
    suite.register(&writer, "a").unwrap();
    let msg = ExecuteMsg::DeregisterInstance {
        instance_id: "a".to_string(),
    };
    let response = suite.exec(&other, &msg).unwrap();
    assert!(!response
        .events
        .iter()
        .any(|e| e.ty == "wasm-instance_deregistered"));
    assert_eq!(suite.member_ids(&writer), ["a"]);
    let response = suite.exec(&writer, &msg).unwrap();
    assert!(response
        .events
        .iter()
        .any(|e| e.ty == "wasm-instance_deregistered"));
    assert!(suite.member(&writer).is_empty());
}

/// instance_id must be [a-z0-9-]+ and at most 32 bytes.
#[test]
fn instance_id_validation() {
    let writer = addr("writer");
    let mut suite = Suite::new(&[&writer]);
    let too_long = "a".repeat(33);
    for bad in [
        "",
        "UPPER",
        "under_score",
        "has space",
        "dot.ted",
        too_long.as_str(),
    ] {
        assert_err(
            suite.register(&writer, bad),
            ContractError::BadInstanceId {},
        );
    }
    suite.register(&writer, &"a".repeat(32)).unwrap();
    suite.register(&writer, "gw-1").unwrap();
}

/// endpoint must be https://host:port and at most max_endpoint_len bytes.
#[test]
fn endpoint_validation() {
    let writer = addr("writer");
    let mut suite = Suite::new(&[&writer]);
    let spki = HexBinary::from([1u8; 32]);
    let too_long = format!("https://{}:443", "a".repeat(250));
    for bad in [
        "",
        "http://gw.example.com:8443",
        "https://gw.example.com",
        "https://:8443",
        "https://gw.example.com:",
        "https://gw.example.com:0",
        "https://gw.example.com:65536",
        "https://gw.example.com:+443",
        "https://gw.example.com:8443/path",
        "https://user@gw.example.com:8443",
        "https://[]:8443",
        too_long.as_str(),
    ] {
        assert_err(
            suite.exec(&writer, &register_with(bad, spki.clone(), vec![])),
            ContractError::BadEndpoint {},
        );
    }
    for (i, good) in ["https://10.0.0.1:443", "https://[fe80::1]:8443"]
        .iter()
        .enumerate()
    {
        suite
            .exec(
                &writer,
                &ExecuteMsg::RegisterInstance {
                    instance_id: format!("ok-{i}"),
                    endpoint: good.to_string(),
                    spki_sha256: spki.clone(),
                    protocol: 1,
                    topics: vec![],
                },
            )
            .unwrap();
    }
}

/// spki_sha256 must be exactly 32 bytes.
#[test]
fn spki_validation() {
    let writer = addr("writer");
    let mut suite = Suite::new(&[&writer]);
    for len in [0usize, 31, 33] {
        assert_err(
            suite.exec(
                &writer,
                &register_with(
                    "https://gw.example.com:8443",
                    HexBinary::from(vec![1u8; len]),
                    vec![],
                ),
            ),
            ContractError::BadFingerprint {},
        );
    }
}

/// Each topic must be [a-z0-9_]+@[0-9]+ and at most 32 bytes, with at most max_topics of them.
#[test]
fn topic_validation() {
    let writer = addr("writer");
    let mut suite = Suite::new(&[&writer]);
    let spki = HexBinary::from([1u8; 32]);
    let endpoint = "https://gw.example.com:8443";
    let too_long = format!("{}@1", "a".repeat(31));
    for bad in [
        "",
        "load",
        "@1",
        "load@",
        "Load@1",
        "lo-ad@1",
        "load@v1",
        "load@1@2",
        too_long.as_str(),
    ] {
        assert_err(
            suite.exec(
                &writer,
                &register_with(endpoint, spki.clone(), vec![bad.to_string()]),
            ),
            ContractError::BadTopic {
                topic: bad.to_string(),
            },
        );
    }
    assert_err(
        suite.exec(
            &writer,
            &register_with(endpoint, spki.clone(), vec!["load@1".to_string(); 17]),
        ),
        ContractError::TooManyTopics {},
    );
    let max_len_topic = format!("{}@1", "a".repeat(30));
    suite
        .exec(
            &writer,
            &register_with(
                endpoint,
                spki,
                vec![max_len_topic, "load_v2@10".to_string()],
            ),
        )
        .unwrap();
}

/// MembersOf answers for 100 addresses and refuses 101.
#[test]
fn members_of_limit() {
    let writer = addr("writer");
    let mut suite = Suite::new(&[&writer]);
    suite.register(&writer, "a").unwrap();
    let mut addresses: Vec<String> = (0..99)
        .map(|i| addr(&format!("a{i}")).to_string())
        .collect();
    addresses.push(writer.to_string());

    let of: Vec<(String, Vec<Instance>)> = suite
        .app
        .wrap()
        .query_wasm_smart(
            &suite.contract,
            &QueryMsg::MembersOf {
                addresses: addresses.clone(),
            },
        )
        .unwrap();
    assert_eq!(of.len(), 100);
    assert_eq!(of[99].1.len(), 1);

    addresses.push(addr("extra").to_string());
    let err = suite
        .app
        .wrap()
        .query_wasm_smart::<Vec<(String, Vec<Instance>)>>(
            &suite.contract,
            &QueryMsg::MembersOf { addresses },
        )
        .unwrap_err();
    assert!(err
        .to_string()
        .contains(&ContractError::TooManyAddresses {}.to_string()));
}

/// Writers pages through the list and caps the page at 100.
#[test]
fn writers_pagination() {
    let writers: Vec<Addr> = (0..105).map(|i| addr(&format!("w{i}"))).collect();
    let refs: Vec<&Addr> = writers.iter().collect();
    let suite = Suite::new(&refs);
    let page: Vec<String> = suite
        .app
        .wrap()
        .query_wasm_smart(
            &suite.contract,
            &QueryMsg::Writers {
                start_after: None,
                limit: Some(1000),
            },
        )
        .unwrap();
    assert_eq!(page.len(), 100);
    let rest: Vec<String> = suite
        .app
        .wrap()
        .query_wasm_smart(
            &suite.contract,
            &QueryMsg::Writers {
                start_after: page.last().cloned(),
                limit: None,
            },
        )
        .unwrap();
    assert_eq!(rest.len(), 5);
    assert!(!page.contains(&rest[0]));
}

/// Members pages by (address, instance_id) and an expired row does not shorten a page.
#[test]
fn members_pagination_skips_expired() {
    let writer = addr("writer");
    let mut suite = Suite::new(&[&writer]);
    suite.register(&writer, "a-old").unwrap();
    suite.advance(1);
    for id in ["b", "c", "d"] {
        suite.register(&writer, id).unwrap();
    }
    suite.advance(TTL - 1);

    let page: Vec<(String, Instance)> = suite
        .app
        .wrap()
        .query_wasm_smart(
            &suite.contract,
            &QueryMsg::Members {
                start_after: None,
                limit: Some(2),
            },
        )
        .unwrap();
    let ids: Vec<&str> = page.iter().map(|(_, i)| i.instance_id.as_str()).collect();
    assert_eq!(ids, ["b", "c"]);
    let page: Vec<(String, Instance)> = suite
        .app
        .wrap()
        .query_wasm_smart(
            &suite.contract,
            &QueryMsg::Members {
                start_after: Some((writer.to_string(), "c".to_string())),
                limit: Some(2),
            },
        )
        .unwrap();
    let ids: Vec<&str> = page.iter().map(|(_, i)| i.instance_id.as_str()).collect();
    assert_eq!(ids, ["d"]);
}

fn instantiate_with(
    max_instances_per_address: u32,
    min_update_interval_blocks: u64,
) -> StdResult<Addr> {
    let mut app = App::default();
    let owner = app.api().addr_make("owner");
    let code_id = app.store_code(contract());
    app.instantiate_contract(
        code_id,
        owner.clone(),
        &InstantiateMsg {
            owner: owner.to_string(),
            writers: vec![],
            max_endpoint_len: 256,
            max_topics: 16,
            max_instances_per_address,
            min_update_interval_blocks,
        },
        &[],
        "gateway-registry",
        None,
    )
}

fn valid_config() -> Config {
    Config {
        owner: addr("owner"),
        paused: false,
        max_endpoint_len: 256,
        max_topics: 16,
        max_instances_per_address: 16,
        min_update_interval_blocks: MIN_INTERVAL,
        instance_ttl_blocks: TTL,
        prune_per_write: 10,
        writers_source: WritersSource::ContractCopy,
    }
}

fn assert_invalid_config(result: Result<(), ContractError>, reason: &str) {
    assert_eq!(
        result.unwrap_err().to_string(),
        ContractError::InvalidConfig {
            reason: reason.to_string()
        }
        .to_string()
    );
}

// Test flow:
// 1. Instantiate with max_instances_per_address 0.
// 2. Check the InvalidConfig rejection.
#[test]
fn instantiate_rejects_zero_max_instances() {
    assert_err(
        instantiate_with(0, MIN_INTERVAL).map(|_| AppResponse::default()),
        ContractError::InvalidConfig {
            reason: "max_instances_per_address must be positive".to_string(),
        },
    );
}

// Test flow:
// 1. Instantiate with min_update_interval_blocks equal to the ttl and check the rejection.
// 2. Instantiate with it above the ttl and check the rejection.
// 3. Instantiate with it one block below the ttl and check it succeeds.
#[test]
fn instantiate_rejects_interval_not_below_ttl() {
    let expected = || ContractError::InvalidConfig {
        reason: "min_update_interval_blocks must be below instance_ttl_blocks".to_string(),
    };
    assert_err(
        instantiate_with(16, TTL).map(|_| AppResponse::default()),
        expected(),
    );
    assert_err(
        instantiate_with(16, TTL + 1).map(|_| AppResponse::default()),
        expected(),
    );
    instantiate_with(16, TTL - 1).unwrap();
}

// Test flow:
// 1. Validate a config with instance_ttl_blocks 0.
// 2. Check the InvalidConfig rejection.
#[test]
fn validate_config_rejects_zero_ttl() {
    let config = Config {
        instance_ttl_blocks: 0,
        ..valid_config()
    };
    assert_invalid_config(
        validate_config(&config),
        "instance_ttl_blocks must be positive",
    );
}

// Test flow:
// 1. Validate a config with prune_per_write 0.
// 2. Check the InvalidConfig rejection.
#[test]
fn validate_config_rejects_zero_prune_per_write() {
    let config = Config {
        prune_per_write: 0,
        ..valid_config()
    };
    assert_invalid_config(validate_config(&config), "prune_per_write must be positive");
}

// Test flow:
// 1. Store an older cw2 version in mock storage.
// 2. Call migrate.
// 3. Check the stored name and version are this crate's.
#[test]
fn migrate_sets_contract_version() {
    let mut deps = mock_dependencies();
    cw2::set_contract_version(deps.as_mut().storage, "gateway-registry", "0.0.1").unwrap();

    migrate(deps.as_mut(), mock_env(), MigrateMsg {}).unwrap();

    let stored = cw2::get_contract_version(deps.as_ref().storage).unwrap();
    assert_eq!(stored.contract, "gateway-registry");
    assert_eq!(stored.version, env!("CARGO_PKG_VERSION"));
}
