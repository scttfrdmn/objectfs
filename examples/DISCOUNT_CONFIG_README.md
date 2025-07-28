# External Discount Configuration for Institutions

ObjectFS supports loading AWS S3 discount configurations from external files, making it easy for institutions to distribute standardized pricing configurations to their users.

## Overview

Many organizations negotiate enterprise agreements with AWS that include:
- Volume discounts based on usage tiers
- Enterprise-wide percentage discounts
- Reserved capacity pricing
- Custom tier-specific rates

Rather than requiring each user to manually configure these discounts, institutions can create a single discount configuration file and distribute it to users.

## How It Works

1. **Institution Creates Discount Config**: The IT department creates a YAML file containing all negotiated AWS discounts
2. **Distribution**: The discount config file is distributed to users (via shared storage, email, institutional software repositories, etc.)
3. **User Configuration**: Users reference the external file in their ObjectFS configuration
4. **Automatic Loading**: ObjectFS automatically loads and applies the institutional discounts

## Configuration Files

### Main ObjectFS Config (`config.yaml`)

```yaml
backends:
  s3:
    # ... other S3 configuration ...
    
    pricing_config:
      # Reference external discount configuration file
      discount_config_file: "discount-config.yaml"  # Can be relative or absolute path
      
      # Optional: inline discount config (merged with external, external takes precedence)
      discount_config:
        enterprise_discount: 10.0  # Fallback if external file unavailable
```

### External Discount Config (`discount-config.yaml`)

See the example `discount-config.yaml` file in this directory for a complete template.

Key sections:
- **Enterprise Discounts**: Overall percentage discounts applied to all storage costs
- **Volume Tiers**: Additional discounts based on total data size
- **Custom Tier Discounts**: Special rates for specific storage tiers
- **Metadata**: Institution information and contract details

## File Distribution Methods

### Method 1: Network Share
Place the discount config on a shared network drive:
```yaml
discount_config_file: "//fileserver/shared/aws/discount-config.yaml"
```

### Method 2: User Home Directory
Distribute via login scripts or configuration management:
```yaml
discount_config_file: "~/aws-discount-config.yaml"
```

### Method 3: System-wide Configuration
Install system-wide (requires admin privileges):
```yaml
discount_config_file: "/etc/objectfs/institutional-discounts.yaml"
```

### Method 4: Relative to ObjectFS Config
Place alongside the main config file:
```yaml
discount_config_file: "discount-config.yaml"  # Same directory as config.yaml
```

## Configuration Merging

When both inline `discount_config` and `discount_config_file` are specified:
- External file values take precedence for non-zero values
- Inline values are used as fallbacks if external file is unavailable
- Volume tiers and custom discounts are completely replaced by external config (not merged)

## Security Considerations

### File Permissions
- Discount config files contain confidential pricing information
- Set appropriate file permissions (readable by ObjectFS users only)
- Consider encryption for highly sensitive discount rates

### Path Resolution
- Relative paths are resolved relative to the current working directory
- Use absolute paths for system-wide configurations
- Validate file paths in production environments

## Example Institution Workflow

1. **IT Department**: Creates `university-aws-discounts.yaml` with negotiated rates
2. **Distribution**: Places file on shared storage or distributes via configuration management
3. **User Setup**: Users add `discount_config_file: "/shared/aws/university-aws-discounts.yaml"` to their config
4. **Automatic Updates**: IT can update discount rates centrally without requiring user reconfiguration

## Validation and Testing

### Verify Configuration Loading
Check ObjectFS logs for successful loading:
```
INFO: Loaded external discount configuration file=/path/to/discount-config.yaml
```

### Test Pricing Calculations
Use ObjectFS cost estimation features to verify discounts are applied correctly.

### Handle Missing Files
ObjectFS will gracefully fall back to inline configuration if the external file is unavailable, logging a warning.

## Troubleshooting

### Common Issues

**File Not Found**
```
WARN: Failed to load external discount config file, using inline config
```
- Verify file path is correct
- Check file permissions
- Ensure file exists and is readable

**YAML Parse Error**
```
ERROR: Failed to parse discount config YAML
```
- Validate YAML syntax using online validators
- Check for correct indentation (spaces, not tabs)
- Verify field names match expected structure

**Discount Not Applied**
- Check that volume tiers have correct size thresholds
- Verify tier names match ObjectFS constants (STANDARD, STANDARD_IA, etc.)
- Ensure discount percentages are reasonable (0-100)

### Debug Tips

1. Enable debug logging to see detailed discount calculations
2. Compare costs with and without external config
3. Use ObjectFS pricing summary features to verify loaded discounts

## Schema Reference

The external discount configuration file supports the same schema as the inline `discount_config` section. See the main ObjectFS documentation for complete field definitions.

## Support

For questions about external discount configuration:
1. Check ObjectFS documentation
2. Review example files in this directory
3. Contact your institution's IT support for organization-specific discount rates