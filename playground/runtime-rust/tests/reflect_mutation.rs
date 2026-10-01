mod support;

use mini_go::{
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
    value::Data,
};
use serde_json::json;
use std::sync::Arc;
use support::slot_code;

#[test]
fn nil_slice_header_mutations_preserve_nil_and_reject_out_of_bounds_growth() {
    for intrinsic in ["reflect.value_set_len", "reflect.value_set_cap"] {
        for (count, named) in [(0, true), (1, true), (-1, true), (0, false)] {
            let view = if named {
                json!({"kind":4,"named":{"module_path":"reflect","decl_id":"Value"}})
            } else {
                json!({"kind":12,"node":"view"})
            };
            let image = support::image(json!({
                "module":{"path":"reflect","package":"reflect"},
                "type_table":{"nodes":[
                    {"id":"slice","kind":5,"elem":{"kind":3,"primitive":3}},
                    {"id":"pointer","kind":8,"elem":{"kind":5,"node":"slice"}},
                    {"id":"interface","kind":13},
                    {"id":"Value","kind":4,"identity":{"module_path":"reflect","decl_id":"Value"},"underlying":{"kind":12,"node":"view"}},
                    {"id":"view","kind":12,"fields":[{"name":"valid","type":{"kind":3,"primitive":1}},{"name":"settable","type":{"kind":3,"primitive":1}},{"name":"target","type":{"kind":2}},{"name":"data","type":{"kind":2}},{"name":"valueType","type":{"kind":13,"node":"interface"}}]}
                ]},
                "constants":[{"id":"true","type":{"kind":3,"primitive":1},"value":true},{"id":"count","type":{"kind":3,"primitive":3},"value":count}],
                "functions":[{"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":2},{"kind":3,"primitive":1},{"kind":5,"node":"slice"}]},
                    "locals":[{"id":"slice","type":{"kind":5,"node":"slice"}}],
                    "code":slot_code(json!([{"kind":3,"primitive":1},{"kind":3,"primitive":1},{"kind":8,"node":"pointer"},{"kind":2},{"kind":13,"node":"interface"},view,{"kind":3,"primitive":3},{"kind":3,"primitive":2},{"kind":3,"primitive":1},{"kind":5,"node":"slice"}]), &[
                        ("const",json!({"constant":"true"}),json!({"outputs":[0]})),
                        ("const",json!({"constant":"true"}),json!({"outputs":[1]})),
                        ("address_of",json!({"kind":"local","local":"slice"}),json!({"outputs":[2]})),
                        ("zero",json!({"type":{"kind":2}}),json!({"outputs":[3]})),
                        ("zero",json!({"type":{"kind":13,"node":"interface"}}),json!({"outputs":[4]})),
                        ("make_struct",json!({"type":view,"fields":["valid","settable","target","data","valueType"]}),json!({"inputs":[[0,0],[0,1],[0,2],[0,3],[0,4]],"outputs":[5],"release":[0,1,2,3,4]})),
                        ("const",json!({"constant":"count"}),json!({"outputs":[6]})),
                        ("call_intrinsic",json!({"id":intrinsic,"arg_count":2,"result_count":2}),json!({"inputs":[[0,5],[0,6]],"outputs":[7,8],"release":[5,6]})),
                        ("load_local",json!({"local":"slice"}),json!({"outputs":[9]})),
                        ("return",json!({"result_count":3}),json!({"inputs":[[0,7],[0,8],[0,9]],"release":[7,8,9]}))])}]
            }));
            let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
            let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
            instance.start("default", vec![]).unwrap();
            assert_eq!(instance.poll_steps(10).unwrap(), PollStatus::Ready);
            assert!(
                matches!(instance.results()[1].data(), Data::Bool(success) if *success == (count == 0 && named)),
                "{intrinsic}({count}): {}",
                instance.results()[0]
            );
            assert!(matches!(instance.results()[2].data(), Data::Nil));
            instance.close().unwrap();
            assert_eq!(instance.heap_stats().live_bytes, 0);
        }
    }
}
