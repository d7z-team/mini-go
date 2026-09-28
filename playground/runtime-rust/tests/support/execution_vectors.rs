use serde::de::DeserializeOwned;

pub fn load<T: DeserializeOwned>() -> Vec<T> {
    serde_json::from_reader(std::io::BufReader::new(flate2::read::GzDecoder::new(
        &include_bytes!("../../../../testdata/runtime/execution.json.gz")[..],
    )))
    .unwrap()
}
