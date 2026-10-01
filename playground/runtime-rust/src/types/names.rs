#[path = "names_generated.rs"]
mod tables;

fn contains_scalar(ranges: &[[u32; 3]], ch: char) -> bool {
    let value = u32::from(ch);
    let index = ranges.partition_point(|row| row[1] < value);
    ranges
        .get(index)
        .is_some_and(|row| value >= row[0] && (value - row[0]) % row[2] == 0)
}

pub(crate) fn is_exported(name: &str) -> bool {
    name.chars().next().is_some_and(|ch| {
        ch.is_ascii_uppercase() || !ch.is_ascii() && contains_scalar(tables::UPPER, ch)
    })
}

pub(crate) fn is_identifier(name: &str) -> bool {
    !name.is_empty()
        && name.chars().enumerate().all(|(index, ch)| {
            ch == '_'
                || ch.is_ascii_alphabetic()
                || !ch.is_ascii() && contains_scalar(tables::LETTER, ch)
                || index > 0
                    && (ch.is_ascii_digit() || !ch.is_ascii() && contains_scalar(tables::DIGIT, ch))
        })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn frozen_unicode_lookup_covers_every_scalar() {
        let mut expected = vec![0u8; 0x110000];
        for (index, ranges) in [tables::LETTER, tables::DIGIT, tables::UPPER]
            .iter()
            .enumerate()
        {
            for &[start, end, stride] in *ranges {
                for value in (start..=end).step_by(stride as usize) {
                    expected[value as usize] |= 1 << index;
                }
            }
        }
        let mut buffer = [0u8; 4];
        for (value, flags) in expected.into_iter().enumerate() {
            let Some(ch) = char::from_u32(value as u32) else {
                continue;
            };
            let text = ch.encode_utf8(&mut buffer);
            assert_eq!(
                is_identifier(text),
                ch == '_' || flags & 1 != 0,
                "U+{value:04X}"
            );
            assert_eq!(is_exported(text), flags & 4 != 0, "U+{value:04X}");
            assert_eq!(
                contains_scalar(tables::DIGIT, ch),
                flags & 2 != 0,
                "U+{value:04X}"
            );
        }
        assert!(is_identifier("a\u{11f50}"));
        assert!(!is_identifier("a²"));
        assert!(!is_identifier("Ⅳ"));
        assert!(is_identifier("func"));
        assert!(!is_identifier(""));
    }
}
