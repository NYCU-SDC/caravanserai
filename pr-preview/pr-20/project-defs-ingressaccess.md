# Untitled object in Project Schema

```txt
project.json#/$defs/IngressDef/properties/access
```

IngressAccess defines visibility and auth rules for an ingress endpoint.

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                          |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [project.json\*](../../schemas/project.json "open original schema") |

## access Type

`object` ([Details](project-defs-ingressaccess.md))

# access Properties

| Property        | Type     | Required | Nullable       | Defined by                                                                                                     |
| :-------------- | :------- | :------- | :------------- | :------------------------------------------------------------------------------------------------------------- |
| [scope](#scope) | `string` | Required | cannot be null | [Project](project-defs-ingressaccess-properties-scope.md "project.json#/$defs/IngressAccess/properties/scope") |

## scope

Controls whether a route is exposed to the public internet or only to the Headscale overlay network.

`scope`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-ingressaccess-properties-scope.md "project.json#/$defs/IngressAccess/properties/scope")

### scope Type

`string`

### scope Constraints

**enum**: the value of this property must be equal to one of the following values:

| Value        | Explanation |
| :----------- | :---------- |
| `"Internal"` |             |
