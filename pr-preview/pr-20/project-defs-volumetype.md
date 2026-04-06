# Untitled string in Project Schema

```txt
project.json#/$defs/VolumeType
```

Governs the lifecycle and backup behaviour of a Volume.

| Abstract            | Extensible | Status         | Identifiable            | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                          |
| :------------------ | :--------- | :------------- | :---------------------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | Unknown identifiability | Forbidden         | Allowed               | none                | [project.json\*](../../schemas/project.json "open original schema") |

## VolumeType Type

`string`

## VolumeType Constraints

**enum**: the value of this property must be equal to one of the following values:

| Value         | Explanation |
| :------------ | :---------- |
| `"Ephemeral"` |             |
