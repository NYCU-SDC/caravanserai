# Untitled object in Project Schema

```txt
project.json#/$defs/ProjectSpec/properties/ingress/items
```

IngressDef describes a single HTTP ingress rule for a Project.

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                          |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [project.json\*](../../schemas/project.json "open original schema") |

## items Type

`object` ([Details](project-defs-ingressdef.md))

# items Properties

| Property          | Type     | Required | Nullable       | Defined by                                                                                             |
| :---------------- | :------- | :------- | :------------- | :----------------------------------------------------------------------------------------------------- |
| [name](#name)     | `string` | Required | cannot be null | [Project](project-defs-ingressdef-properties-name.md "project.json#/$defs/IngressDef/properties/name") |
| [host](#host)     | `string` | Optional | cannot be null | [Project](project-defs-ingressdef-properties-host.md "project.json#/$defs/IngressDef/properties/host") |
| [target](#target) | `object` | Required | cannot be null | [Project](project-defs-ingresstarget.md "project.json#/$defs/IngressDef/properties/target")            |
| [access](#access) | `object` | Optional | cannot be null | [Project](project-defs-ingressaccess.md "project.json#/$defs/IngressDef/properties/access")            |

## name



`name`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-ingressdef-properties-name.md "project.json#/$defs/IngressDef/properties/name")

### name Type

`string`

## host



`host`

* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-ingressdef-properties-host.md "project.json#/$defs/IngressDef/properties/host")

### host Type

`string`

## target

IngressTarget is the backend service and port for an ingress rule.

`target`

* is required

* Type: `object` ([Details](project-defs-ingresstarget.md))

* cannot be null

* defined in: [Project](project-defs-ingresstarget.md "project.json#/$defs/IngressDef/properties/target")

### target Type

`object` ([Details](project-defs-ingresstarget.md))

## access

IngressAccess defines visibility and auth rules for an ingress endpoint.

`access`

* is optional

* Type: `object` ([Details](project-defs-ingressaccess.md))

* cannot be null

* defined in: [Project](project-defs-ingressaccess.md "project.json#/$defs/IngressDef/properties/access")

### access Type

`object` ([Details](project-defs-ingressaccess.md))
