use mini_go::heap::{Handle, Heap, Trace};

#[derive(Debug, PartialEq)]
struct Node {
    value: usize,
    edges: Vec<Handle>,
}

impl Trace for Node {
    fn stable_edges(&self) -> bool {
        true
    }
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        for edge in &self.edges {
            visit(*edge);
        }
    }
}

#[test]
fn replacements_update_cached_reachability_and_preserve_read_snapshots() {
    let heap = Heap::new(4, 512).unwrap();
    let first = heap
        .allocate(
            Node {
                value: 1,
                edges: Vec::new(),
            },
            128,
        )
        .unwrap();
    let root = heap
        .allocate(
            Node {
                value: 0,
                edges: vec![first],
            },
            128,
        )
        .unwrap();
    heap.collect([root]).unwrap();
    let second = heap
        .allocate(
            Node {
                value: 2,
                edges: Vec::new(),
            },
            128,
        )
        .unwrap();
    let before = heap.get(root).unwrap();
    heap.replace(
        root,
        Node {
            value: before.value,
            edges: vec![second],
        },
        128,
    )
    .unwrap();
    assert_eq!(before.edges, vec![first]);
    heap.collect([root]).unwrap();
    assert!(heap.get(first).is_err());
    assert_eq!(heap.get(second).unwrap().value, 2);
    heap.replace(
        root,
        Node {
            value: 3,
            edges: Vec::new(),
        },
        128,
    )
    .unwrap();
    heap.collect([root]).unwrap();
    assert!(heap.get(second).is_err());
    assert_eq!(heap.get(root).unwrap().value, 3);
}

#[test]
fn interior_mutable_graphs_are_retraced_without_mutable_heap_access() {
    #[derive(Debug)]
    struct Dynamic(std::cell::RefCell<Vec<Handle>>);
    impl Trace for Dynamic {
        fn trace(&self, visit: &mut dyn FnMut(Handle)) {
            for handle in self.0.borrow().iter() {
                visit(*handle);
            }
        }
    }
    let heap = Heap::new(3, 384).unwrap();
    let root = heap
        .allocate(Dynamic(std::cell::RefCell::new(Vec::new())), 128)
        .unwrap();
    heap.collect([root]).unwrap();
    let child = heap
        .allocate(Dynamic(std::cell::RefCell::new(Vec::new())), 128)
        .unwrap();
    heap.get(root).unwrap().0.borrow_mut().push(child);
    heap.collect([root]).unwrap();
    assert!(heap.get(child).is_ok());
    heap.get(root).unwrap().0.borrow_mut().clear();
    heap.collect([root]).unwrap();
    assert!(heap.get(child).is_err());
}

#[test]
fn replacement_quota_failure_preserves_object_and_accounting() {
    let heap = Heap::new(2, 256).unwrap();
    let handle = heap
        .allocate(
            Node {
                value: 1,
                edges: Vec::new(),
            },
            128,
        )
        .unwrap();
    let before = heap.stats();
    let (error, replacement) = heap
        .replace(
            handle,
            Node {
                value: 2,
                edges: vec![handle],
            },
            257,
        )
        .unwrap_err();
    assert_eq!(error.code, "allocation_limit");
    assert_eq!(replacement.value, 2);
    assert_eq!(heap.get(handle).unwrap().value, 1);
    assert_eq!(heap.stats(), before);
    heap.replace(handle, replacement, 256).unwrap();
    assert_eq!(heap.get(handle).unwrap().edges, [handle]);
    assert_eq!(heap.collect([handle]).unwrap(), 0);
    assert_eq!(heap.collect([]).unwrap(), 1);
}

#[test]
fn cycles_are_retained_by_roots_and_reclaimed_without_roots() {
    let heap = Heap::new(4, 512).unwrap();
    let a = heap
        .allocate(
            Node {
                value: 1,
                edges: vec![],
            },
            128,
        )
        .unwrap();
    let b = heap
        .allocate(
            Node {
                value: 2,
                edges: vec![a],
            },
            128,
        )
        .unwrap();
    heap.replace(
        a,
        Node {
            value: 1,
            edges: vec![b],
        },
        128,
    )
    .unwrap();
    assert_eq!(heap.collect([a, a]).unwrap(), 0);
    assert_eq!(heap.stats().live_bytes, 256);
    assert_eq!(heap.collect([]).unwrap(), 2);
    assert_eq!(heap.stats().live_bytes, 0);
    let replacement = heap
        .allocate(
            Node {
                value: 3,
                edges: vec![],
            },
            128,
        )
        .unwrap();
    assert!(heap.get(a).is_err());
    assert!(heap.get(b).is_err());
    assert_eq!(heap.get(replacement).unwrap().value, 3);
}

#[test]
fn failed_allocation_preserves_input_and_existing_graph() {
    let heap = Heap::new(3, 128).unwrap();
    let root = heap
        .allocate(
            Node {
                value: 1,
                edges: vec![],
            },
            128,
        )
        .unwrap();
    let before = heap.stats();
    let (error, value) = heap
        .allocate(
            Node {
                value: 2,
                edges: vec![root],
            },
            1,
        )
        .unwrap_err();
    assert_eq!(error.code, "allocation_limit");
    assert_eq!(value.edges, [root]);
    assert_eq!(heap.stats(), before);
    assert_eq!(heap.get(root).unwrap().value, 1);
}

#[test]
fn failed_mark_does_not_partially_collect_live_objects() {
    let heap = Heap::new(4, 512).unwrap();
    let stale = heap
        .allocate(
            Node {
                value: 0,
                edges: vec![],
            },
            128,
        )
        .unwrap();
    heap.collect([]).unwrap();
    let root = heap
        .allocate(
            Node {
                value: 1,
                edges: vec![stale],
            },
            128,
        )
        .unwrap();
    let before = heap.stats();
    assert_eq!(heap.collect([root]).unwrap_err().code, "stale_reference");
    assert_eq!(heap.stats(), before);
    assert_eq!(heap.get(root).unwrap().value, 1);
}

#[test]
fn handles_are_scoped_to_their_heap() {
    let left = Heap::new(2, 256).unwrap();
    let right = Heap::new(2, 256).unwrap();
    let a = left
        .allocate(
            Node {
                value: 1,
                edges: vec![],
            },
            128,
        )
        .unwrap();
    let b = right
        .allocate(
            Node {
                value: 2,
                edges: vec![],
            },
            128,
        )
        .unwrap();
    assert!(left.get(b).is_err());
    assert!(right.get(a).is_err());
    assert_eq!(left.collect([b]).unwrap_err().code, "stale_reference");
    assert_eq!(left.get(a).unwrap().value, 1);
}
