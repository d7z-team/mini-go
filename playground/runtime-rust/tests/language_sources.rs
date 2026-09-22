use mini_go::ffi::Cancellation;
use mini_go::language::read_directory;

struct Directory(std::path::PathBuf);
impl Drop for Directory {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

#[tokio::test]
async fn directory_sources_preserve_binary_files_and_cancellation() {
    let root = Directory(std::env::temp_dir().join(format!(
            "mini-go-sources-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        )));
    std::fs::create_dir_all(root.0.join("vendor")).unwrap();
    std::fs::create_dir(root.0.join(".git")).unwrap();
    std::fs::write(root.0.join("data.bin"), [0, 255, 1]).unwrap();
    std::fs::write(root.0.join("vendor/value.mgo"), "package vendor\n").unwrap();
    std::fs::write(root.0.join(".git/config"), "private").unwrap();
    let tree = read_directory("app", &root.0, &Cancellation::default())
        .await
        .unwrap();
    assert_eq!(
        tree.files
            .iter()
            .map(|file| file.path.as_str())
            .collect::<Vec<_>>(),
        ["data.bin", "vendor/value.mgo"]
    );
    assert_eq!(tree.files[0].data, "AP8B");
    let canceled = Cancellation::default();
    canceled.cancel();
    assert_eq!(
        read_directory("app", &root.0, &canceled)
            .await
            .unwrap_err()
            .code,
        "canceled"
    );
}
