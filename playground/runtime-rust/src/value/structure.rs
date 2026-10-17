//! Struct value copies share immutable fields until a destination is written.

use super::Value;
use std::{collections::BTreeMap, ops::Index, sync::Arc};

#[derive(Clone, Debug)]
pub struct StructStorage {
    names: Arc<Vec<String>>,
    fields: Arc<Vec<Value>>,
}

impl StructStorage {
    pub(crate) fn with_layout(names: Arc<Vec<String>>, values: Vec<Value>) -> Self {
        debug_assert_eq!(names.len(), values.len());
        Self {
            names,
            fields: Arc::new(values),
        }
    }
    pub fn get_mut(&mut self, name: &str) -> Option<&mut Value> {
        let index = self.field_index(name)?;
        Arc::make_mut(&mut self.fields).get_mut(index)
    }

    pub(crate) fn field_index(&self, name: &str) -> Option<usize> {
        self.names.iter().position(|candidate| candidate == name)
    }

    pub(crate) fn field_at(&self, index: usize) -> Option<&Value> {
        self.fields.get(index)
    }

    pub(crate) fn field_at_mut(&mut self, index: usize) -> Option<&mut Value> {
        Arc::make_mut(&mut self.fields).get_mut(index)
    }

    pub fn get(&self, name: &str) -> Option<&Value> {
        self.field_at(self.field_index(name)?)
    }

    pub fn contains_key(&self, name: &str) -> bool {
        self.field_index(name).is_some()
    }

    pub fn len(&self) -> usize {
        self.fields.len()
    }

    pub fn is_empty(&self) -> bool {
        self.fields.is_empty()
    }

    pub fn values(&self) -> std::slice::Iter<'_, Value> {
        self.fields.iter()
    }

    pub fn keys(&self) -> std::slice::Iter<'_, String> {
        self.names.iter()
    }

    pub fn iter(
        &self,
    ) -> std::iter::Zip<std::slice::Iter<'_, String>, std::slice::Iter<'_, Value>> {
        self.names.iter().zip(self.fields.iter())
    }
}

impl From<BTreeMap<String, Value>> for StructStorage {
    fn from(fields: BTreeMap<String, Value>) -> Self {
        let (names, values): (Vec<_>, Vec<_>) = fields.into_iter().unzip();
        Self::with_layout(Arc::new(names), values)
    }
}

impl Index<&str> for StructStorage {
    type Output = Value;
    fn index(&self, name: &str) -> &Value {
        self.get(name).expect("field belongs to struct layout")
    }
}

impl IntoIterator for StructStorage {
    type Item = (String, Value);
    type IntoIter = std::iter::Zip<std::vec::IntoIter<String>, std::vec::IntoIter<Value>>;

    fn into_iter(self) -> Self::IntoIter {
        Arc::unwrap_or_clone(self.names)
            .into_iter()
            .zip(Arc::unwrap_or_clone(self.fields))
    }
}

impl<'a> IntoIterator for &'a StructStorage {
    type Item = (&'a String, &'a Value);
    type IntoIter = std::iter::Zip<std::slice::Iter<'a, String>, std::slice::Iter<'a, Value>>;

    fn into_iter(self) -> Self::IntoIter {
        self.iter()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{types::TypeIdentity, value::Data};

    #[test]
    fn nested_arrays_and_fields_are_independent_after_value_copy() {
        let original = StructStorage::from(BTreeMap::from([
            ("Name".into(), Value::string(b"original".to_vec())),
            (
                "Items".into(),
                Value {
                    typ: TypeIdentity::Any,
                    data: Data::Array(vec![Value::int(42)].into()),
                },
            ),
        ]));
        let mut copy = original.clone();
        let Data::Array(items) = &mut copy.get_mut("Items").unwrap().data else {
            panic!("expected array")
        };
        Arc::make_mut(items)[0] = Value::int(99);
        *copy.get_mut("Name").unwrap() = Value::string(b"copy".to_vec());
        let Data::Array(items) = &original["Items"].data else {
            panic!("expected array")
        };
        assert_eq!(items[0].integer().unwrap(), 42);
        assert_eq!(original["Name"].to_string(), "original");
        assert_eq!(copy["Name"].to_string(), "copy");
    }
}
