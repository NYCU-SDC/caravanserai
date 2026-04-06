# Untitled object in Project Schema

```txt
project.json#/$defs/ObjectMeta/properties/labels
```

Labels are arbitrary key/value pairs used for selection and grouping.

| Abstract            | Extensible | Status         | Identifiable            | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                          |
| :------------------ | :--------- | :------------- | :---------------------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | Unknown identifiability | Forbidden         | Allowed               | none                | [project.json\*](../../schemas/project.json "open original schema") |

## labels Type

`object` ([Details](project-defs-objectmeta-properties-labels.md))

# labels Properties

| Property              | Type     | Required | Nullable       | Defined by                                                                                                                                           |
| :-------------------- | :------- | :------- | :------------- | :--------------------------------------------------------------------------------------------------------------------------------------------------- |
| Additional Properties | `string` | Optional | cannot be null | [Project](project-defs-objectmeta-properties-labels-additionalproperties.md "project.json#/$defs/ObjectMeta/properties/labels/additionalProperties") |

## Additional Properties

Additional properties are allowed, as long as they follow this schema:



* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-objectmeta-properties-labels-additionalproperties.md "project.json#/$defs/ObjectMeta/properties/labels/additionalProperties")

### additionalProperties Type

`string`
